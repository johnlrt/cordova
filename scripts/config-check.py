#!/usr/bin/env python3
"""Validate the local M1 Compose boundary and development credentials."""

from __future__ import annotations

import json
import re
import stat
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-f", "infra/compose.yaml", "-f", "infra/compose.dev.yaml"]


def fail(message: str) -> None:
    raise ValueError(message)


def main() -> int:
    try:
        result = subprocess.run(COMPOSE + ["config", "--format", "json"], cwd=ROOT, capture_output=True, text=True)
        if result.returncode:
            fail("docker compose config failed; run make bootstrap-dev and inspect the Compose files")
        config = json.loads(result.stdout)
        services = config.get("services", {})
        expected = {"gateway", "mock-node", "redis", "redpanda", "edge"}
        if set(services) != expected:
            fail("Compose service inventory does not match the M1 stack")
        for name, service in services.items():
            ports = service.get("ports", [])
            if name != "edge" and ports:
                fail(f"{name} must not publish a host port")
            for port in ports:
                if port.get("host_ip") != "127.0.0.1" or str(port.get("target")) != "8443" or str(port.get("published")) != "8443":
                    fail("only 127.0.0.1:8443 may be published")
            image = service.get("image", "")
            if not image or image.endswith(":latest") or (":" not in image.rsplit("/", 1)[-1] and "@sha256:" not in image):
                fail(f"{name} image must use an explicit version tag or digest")
            for volume in service.get("volumes", []):
                source = volume.get("source", "") if isinstance(volume, dict) else ""
                if source == "/var/run/docker.sock" or "docker.sock" in source:
                    fail("Docker socket mounts are forbidden")
        for network in config.get("networks", {}).values():
            if not network.get("internal", False):
                fail("all M1 networks must be internal")
        private = config.get("networks", {}).get("private", {})
        if private.get("internal") is not True:
            fail("Redis, Kafka and mock-node require an internal network")

        secret_names = ["app-pepper", "redis-password", "mock-node-token"]
        for name in secret_names:
            path = ROOT / ".local" / name
            if path.is_symlink() or not path.is_file():
                fail(f"required local secret is missing or unsafe: {name}")
            mode = stat.S_IMODE(path.stat().st_mode)
            if mode & 0o077:
                fail(f"local secret permissions must be 0600: {name}")
            if len(path.read_text(encoding="ascii").strip()) != 64:
                fail(f"local secret must contain 32 random bytes in hex form: {name}")

        for name in ["dev-ca.key", "edge.key", "mock-node.key"]:
            path = ROOT / ".local" / name
            if path.is_symlink() or not path.is_file() or stat.S_IMODE(path.stat().st_mode) & 0o077:
                fail(f"private key permissions or file type are unsafe: {name}")
        for cert in ["dev-ca.crt", "edge.crt", "mock-node.crt"]:
            path = ROOT / ".local" / cert
            if path.is_symlink() or not path.is_file():
                fail(f"local certificate is missing or unsafe: {cert}")

        for command in [
            ["openssl", "verify", "-CAfile", ".local/dev-ca.crt", ".local/edge.crt", ".local/mock-node.crt"],
            ["openssl", "x509", "-in", ".local/edge.crt", "-checkhost", "localhost", "-noout"],
            ["openssl", "x509", "-in", ".local/edge.crt", "-checkip", "127.0.0.1", "-noout"],
            ["openssl", "x509", "-in", ".local/mock-node.crt", "-checkhost", "mock-node", "-noout"],
        ]:
            checked = subprocess.run(command, cwd=ROOT, capture_output=True, text=True)
            if checked.returncode:
                fail("local TLS certificate chain or hostname check failed")

        dockerignore = (ROOT / ".dockerignore").read_text(encoding="utf-8")
        if ".local/" not in dockerignore or "**/node_modules/" not in dockerignore:
            fail("Docker build context must exclude secrets and installed dependencies")
        print("PASS: versioned images, private networks, localhost-only edge, protected secrets and TLS identities")
        return 0
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
