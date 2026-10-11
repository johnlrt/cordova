#!/usr/bin/env python3
"""Verify only the local HTTPS edge is published from the M1 Compose stack."""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-f", "infra/compose.yaml", "-f", "infra/compose.dev.yaml"]


def main() -> int:
    config = subprocess.run(COMPOSE + ["config", "--format", "json"], cwd=ROOT, capture_output=True, text=True)
    if config.returncode:
        print("FAIL: Compose configuration is invalid; run make config-check", file=sys.stderr)
        return 1
    services = json.loads(config.stdout)["services"]
    for name, service in services.items():
        if name != "edge" and service.get("ports"):
            print(f"FAIL: {name} publishes a host port", file=sys.stderr)
            return 1
    if "--config-only" in sys.argv:
        print("PASS: static Compose boundary exposes only the localhost HTTPS edge")
        return 0
    edge = subprocess.run(COMPOSE + ["port", "edge", "8443"], cwd=ROOT, capture_output=True, text=True)
    if edge.returncode or not edge.stdout.strip() or any(not item.startswith("127.0.0.1:8443") for item in edge.stdout.splitlines()):
        print("FAIL: edge is not bound exclusively to localhost:8443", file=sys.stderr)
        return 1
    for name, port in [("gateway", "8080"), ("mock-node", "8090"), ("redis", "6379"), ("kafka", "9092")]:
        result = subprocess.run(COMPOSE + ["port", name, port], cwd=ROOT, capture_output=True, text=True)
        if result.returncode == 0 and result.stdout.strip():
            print(f"FAIL: {name}:{port} has a public host binding", file=sys.stderr)
            return 1
    print("PASS: only 127.0.0.1:8443 is published; gateway, mock-node, Redis and Kafka have no host ports")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
