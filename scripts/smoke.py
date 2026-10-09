#!/usr/bin/env python3
"""Exercise registration, API-key auth, SSE, Kafka projection and dashboard over TLS."""

from __future__ import annotations

import http.cookiejar
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
ORIGIN = os.environ.get("CORDOVA_PUBLIC_ORIGIN", "https://localhost:8443").rstrip("/")


def main() -> int:
    try:
        context = ssl.create_default_context(cafile=str(ROOT / ".local/dev-ca.crt"))
        cookies = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), urllib.request.HTTPCookieProcessor(cookies))

        def request(path: str, *, method: str | None = None, data: object | None = None, headers: dict[str, str] | None = None, timeout: float = 15):
            body = None if data is None else json.dumps(data).encode()
            request_headers = {"Accept": "application/json", **(headers or {})}
            if data is not None:
                request_headers["Content-Type"] = "application/json"
            request_method = method or ("POST" if data is not None else "GET")
            if request_method not in {"GET", "HEAD"}:
                request_headers["Origin"] = ORIGIN
            req = urllib.request.Request(ORIGIN + path, data=body, headers=request_headers, method=request_method)
            return opener.open(req, timeout=timeout)

        username = "smoke-" + uuid.uuid4().hex[:16]
        password = "Cordova-smoke-" + uuid.uuid4().hex
        with request("/api/auth/register", data={"username": username, "password": password, "privacy_notice_version": "privacy-v1"}) as response:
            if response.status != 201:
                raise RuntimeError("registration returned an unexpected status")
            session = json.load(response)
        csrf = session["csrf_token"]
        if not any(cookie.name == "__Host-cordova_session" and cookie.secure and cookie.value for cookie in cookies):
            raise RuntimeError("registration did not set the secure host-only session cookie")

        with request("/api/keys", data={"name": "M1 smoke", "permissions": ["chat:write", "models:read", "usage:read"], "model_ids": ["cordova-chat-small-v1"], "daily_spend_cap_piconero": "0"}, headers={"X-CSRF-Token": csrf}) as response:
            created_key = json.load(response)
        api_key = created_key["secret"]

        with request("/v1/models", headers={"Authorization": "Bearer " + api_key}) as response:
            models = json.load(response)
        if not models.get("data") or models["data"][0].get("id") != "cordova-chat-small-v1":
            raise RuntimeError("model catalog did not return the M1 mock model")
        with request("/api/keys") as response:
            key_rows = json.load(response).get("data", [])
        if not any(row.get("id") == created_key["key"]["id"] and row.get("last_used_at") for row in key_rows):
            raise RuntimeError("API key last-used metadata was not updated")

        sentinel = "M1-SMOKE-" + uuid.uuid4().hex
        payload = {"model": "cordova-chat-small-v1", "messages": [{"role": "user", "content": sentinel}], "stream": True, "stream_options": {"include_usage": True}}
        req = urllib.request.Request(ORIGIN + "/v1/chat/completions", data=json.dumps(payload).encode(), headers={"Authorization": "Bearer " + api_key, "Content-Type": "application/json", "Accept": "text/event-stream"})
        started = time.monotonic()
        with opener.open(req, timeout=30) as response:
            request_id = response.headers.get("X-Request-ID", "")
            if response.headers.get("Content-Type", "").split(";", 1)[0] != "text/event-stream":
                raise RuntimeError("chat response is not an SSE stream")
            first_event = None
            completed = False
            final_usage = None
            for raw_line in response:
                line = raw_line.decode("utf-8").strip()
                if not line.startswith("data: "):
                    continue
                value = line[6:]
                if first_event is None:
                    first_event = time.monotonic()
                if value == "[DONE]":
                    completed = True
                    break
                event = json.loads(value)
                if event.get("error"):
                    raise RuntimeError("SSE returned an error")
                if event.get("choices") == [] and event.get("usage"):
                    final_usage = event["usage"]
        if first_event is None or first_event - started > 5 or not completed or final_usage is None or not request_id:
            raise RuntimeError("SSE did not flush chunks and a final usage record before [DONE]")
        if final_usage.get("total_tokens") != final_usage.get("prompt_tokens", -1) + final_usage.get("completion_tokens", -1):
            raise RuntimeError("SSE usage token totals do not reconcile")

        deadline = time.monotonic() + 30
        projected = None
        while time.monotonic() < deadline:
            with request("/api/usage") as response:
                usage_body = response.read()
                usage_data = json.loads(usage_body)
            if sentinel.encode() in usage_body:
                raise RuntimeError("prompt sentinel leaked into account usage metadata")
            projected = next((row for row in usage_data.get("data", []) if row.get("request_id") == request_id), None)
            if projected is not None:
                break
            time.sleep(0.5)
        if projected is None:
            raise RuntimeError("Kafka projection did not reach the account usage view")
        if projected["input_tokens"] != final_usage["prompt_tokens"] or projected["output_tokens"] != final_usage["completion_tokens"] or projected["charged_piconero"] != "0":
            raise RuntimeError("dashboard projection differs from the SSE completion or contains a charge")
        if projected["api_key_id"] != created_key["key"]["id"]:
            raise RuntimeError("usage projection is not scoped to the API key that made the request")
        with request("/api/usage", headers={"Authorization": "Bearer " + api_key}) as response:
            key_usage = json.load(response)
        if not any(row.get("request_id") == request_id for row in key_usage.get("data", [])):
            raise RuntimeError("usage:read key could not read its own usage metadata")
        with request("/api/keys", data={"name": "M1 restricted smoke", "permissions": ["chat:write", "models:read"], "model_ids": ["cordova-chat-small-v1"], "daily_spend_cap_piconero": "0"}, headers={"X-CSRF-Token": csrf}) as response:
            restricted_record = json.load(response)
            restricted_key = restricted_record["secret"]
        try:
            request("/api/usage", headers={"Authorization": "Bearer " + restricted_key})
            raise RuntimeError("API key without usage:read permission accessed usage")
        except urllib.error.HTTPError as error:
            if error.code != 403:
                raise RuntimeError("usage:read permission denial returned an unexpected status") from error
        with request("/api/keys/" + restricted_record["key"]["id"], method="DELETE", headers={"X-CSRF-Token": csrf}) as response:
            if response.status != 204:
                raise RuntimeError("API key revocation returned an unexpected status")
        try:
            request("/v1/models", headers={"Authorization": "Bearer " + restricted_key})
            raise RuntimeError("revoked API key remained active")
        except urllib.error.HTTPError as error:
            if error.code != 401:
                raise RuntimeError("revoked API key returned an unexpected status") from error

        with request("/dashboard") as response:
            page = response.read()
        if b"Recent requests" not in page or sentinel.encode() in page:
            raise RuntimeError("dashboard deep link is missing or contains prompt data")
        print("PASS: TLS registration, API-key catalog, flushed SSE [DONE], Kafka usage projection and dashboard test-credit view")
        return 0
    except (OSError, KeyError, ValueError, urllib.error.URLError, RuntimeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
