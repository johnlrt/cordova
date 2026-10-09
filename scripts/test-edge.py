#!/usr/bin/env python3
"""Qualify local TLS headers, deep links, denied internals and C4 WebSocket auth."""

from __future__ import annotations

import base64
import http.cookiejar
import json
import os
import socket
import ssl
import struct
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ORIGIN = os.environ.get("CORDOVA_PUBLIC_ORIGIN", "https://localhost:8443").rstrip("/")


class WebSocketStream:
    def __init__(self, connection: ssl.SSLSocket, initial: bytes = b"") -> None:
        self.connection = connection
        self.buffer = bytearray(initial)

    def read(self, count: int) -> bytes:
        while len(self.buffer) < count:
            data = self.connection.recv(max(4096, count - len(self.buffer)))
            if not data:
                raise RuntimeError("WebSocket closed unexpectedly")
            self.buffer.extend(data)
        value = bytes(self.buffer[:count])
        del self.buffer[:count]
        return value

    def send(self, value: dict[str, object]) -> None:
        payload = json.dumps(value, separators=(",", ":")).encode()
        mask = os.urandom(4)
        size = len(payload)
        if size < 126:
            header = bytes([0x81, 0x80 | size])
        elif size <= 65535:
            header = bytes([0x81, 0x80 | 126]) + struct.pack("!H", size)
        else:
            header = bytes([0x81, 0x80 | 127]) + struct.pack("!Q", size)
        masked = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
        self.connection.sendall(header + mask + masked)

    def receive(self) -> dict[str, object]:
        first, second = self.read(2)
        if first & 0x0F != 1 or second & 0x80:
            raise RuntimeError("server sent an unexpected WebSocket frame")
        length = second & 0x7F
        if length == 126:
            length = struct.unpack("!H", self.read(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", self.read(8))[0]
        if length > 1 << 20:
            raise RuntimeError("server WebSocket frame exceeded the protocol bound")
        return json.loads(self.read(length))


def main() -> int:
    try:
        context = ssl.create_default_context(cafile=str(ROOT / ".local/dev-ca.crt"))
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), urllib.request.HTTPCookieProcessor(jar))

        def request(path: str, data: object | None = None, headers: dict[str, str] | None = None):
            body = None if data is None else json.dumps(data).encode()
            request_headers = {"Accept": "application/json", **(headers or {})}
            if data is not None:
                request_headers.update({"Content-Type": "application/json", "Origin": ORIGIN})
            return opener.open(urllib.request.Request(ORIGIN + path, data=body, headers=request_headers), timeout=10)

        with request("/healthz") as response:
            health = json.load(response)
            if health.get("stage") != "m1" or not response.headers.get("Strict-Transport-Security") or not response.headers.get("Content-Security-Policy"):
                raise RuntimeError("TLS edge health headers are incomplete")
        with request("/dashboard") as response:
            if response.status != 200 or b"Recent usage" not in response.read():
                raise RuntimeError("dashboard deep-link refresh failed")
        for path in ["/readyz", "/metrics", "/api/billing/address"]:
            try:
                response = request(path)
                response.close()
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise RuntimeError(f"private path {path} was not denied") from error
            else:
                raise RuntimeError(f"private path {path} was publicly reachable")

        username = "edge-" + uuid.uuid4().hex[:16]
        password = "Cordova-edge-" + uuid.uuid4().hex
        with request("/api/auth/register", {"username": username, "password": password, "privacy_notice_version": "v1"}) as response:
            auth = json.load(response)
        csrf = auth["csrf_token"]
        with request("/api/live-ticket", {}, {"X-CSRF-Token": csrf}) as response:
            ticket = json.load(response)["ticket"]
        cookie = "; ".join(f"{item.name}={item.value}" for item in jar)
        raw = socket.create_connection(("127.0.0.1", 8443), timeout=5)
        secure = context.wrap_socket(raw, server_hostname="localhost")
        secure.settimeout(10)
        key = base64.b64encode(os.urandom(16)).decode()
        secure.sendall((
            "GET /api/live HTTP/1.1\r\nHost: localhost:8443\r\n" +
            f"Origin: {ORIGIN}\r\nCookie: {cookie}\r\n" +
            "Connection: Upgrade\r\nUpgrade: websocket\r\n" +
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
        ).encode())
        response = bytearray()
        while b"\r\n\r\n" not in response:
            response.extend(secure.recv(4096))
        header, remainder = bytes(response).split(b"\r\n\r\n", 1)
        if b" 101 " not in header.split(b"\r\n", 1)[0]:
            raise RuntimeError("same-origin authenticated WebSocket upgrade failed")
        expected = base64.b64encode(__import__("hashlib").sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest())
        if b"Sec-WebSocket-Accept: " + expected not in header:
            raise RuntimeError("WebSocket upgrade accept key did not verify")
        ws = WebSocketStream(secure, remainder)
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        auth_request = str(uuid.uuid4())
        ws.send({"v": 1, "type": "live.auth", "message_id": str(uuid.uuid4()), "request_id": auth_request, "session_id": None, "seq": 1, "sent_at": now, "payload": {"ticket": ticket}, "error": None})
        ready = ws.receive()
        if ready.get("type") != "live.ready" or ready.get("seq") != 1 or ready.get("request_id") != auth_request:
            raise RuntimeError("one-time ticket was not accepted by the C4 live socket")
        session_id = ready["payload"]["session_id"]
        ws.send({"v": 1, "type": "live.subscribe", "message_id": str(uuid.uuid4()), "request_id": str(uuid.uuid4()), "session_id": session_id, "seq": 2, "sent_at": now, "payload": {"topics": ["usage"], "last_event_id": None}, "error": None})
        ping_request = str(uuid.uuid4())
        ws.send({"v": 1, "type": "live.ping", "message_id": str(uuid.uuid4()), "request_id": ping_request, "session_id": session_id, "seq": 3, "sent_at": now, "payload": {"time": now}, "error": None})
        pong = ws.receive()
        if pong.get("type") != "live.pong" or pong.get("request_id") != ping_request or pong.get("seq") != 2:
            raise RuntimeError("C4 application heartbeat did not return a sequenced pong")
        secure.close()
        print("PASS: TLS 1.2+ edge, HSTS/CSP, deep-link refresh, private-route denial, C4 upgrade, one-use ticket and heartbeat")
        return 0
    except (OSError, KeyError, ValueError, urllib.error.URLError, RuntimeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
