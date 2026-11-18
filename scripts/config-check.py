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
        expected = {"gateway", "mock-node", "redis", "redpanda", "usage-topic-init", "edge"}
        if set(services) != expected:
            fail("Compose service inventory does not match the M1 stack")
        topic_init = services["usage-topic-init"]
        topic_command = topic_init.get("command", [])
        required_topic_steps = [
            "rpk topic describe cordova.account.usage.v1 --brokers redpanda:9092",
            "rpk topic create --brokers redpanda:9092 --partitions 1 --replicas 1 cordova.account.usage.v1",
        ]
        if topic_init.get("entrypoint") != ["/bin/sh", "-ec"] or len(topic_command) != 1:
            fail("the M1 usage topic must be explicitly provisioned with stable single-partition ordering")
        if any(step not in topic_command[0] for step in required_topic_steps):
            fail("the M1 usage topic must be explicitly provisioned with stable single-partition ordering")
        topic_dependency = topic_init.get("depends_on", {}).get("redpanda", {})
        gateway_topic_dependency = services["gateway"].get("depends_on", {}).get("usage-topic-init", {})
        if topic_dependency.get("condition") != "service_healthy" or gateway_topic_dependency.get("condition") != "service_completed_successfully":
            fail("the gateway must wait for successful usage topic provisioning")
        redis_script = "\n".join(str(part) for part in services["redis"].get("command", []))
        for setting in ["appendonly yes", "maxmemory-policy noeviction", "stop-writes-on-bgsave-error yes", "save 900 1", "save 300 100", "save 60 10000"]:
            if setting not in redis_script.replace("\\n", " ").replace("\n", " "):
                fail(f"the Redis ledger store must set: {setting}")
        redpanda_command = [str(part) for part in services["redpanda"].get("command", [])]
        if "redpanda.auto_create_topics_enabled=false" not in redpanda_command:
            fail("Redpanda topic auto-creation must stay disabled; topics are provisioned explicitly")
        gateway_environment = services["gateway"].get("environment", {})
        if gateway_environment.get("CORDOVA_PRIVACY_NOTICE_VERSION") != "privacy-v1":
            fail("the gateway must use the current privacy notice version")
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
        networks = config.get("networks", {})
        front = networks.get("front", {})
        private = networks.get("private", {})
        if front.get("internal", False):
            fail("the edge front network must reach the host-published loopback port")
        if private.get("internal") is not True:
            fail("Redis, Kafka and mock-node require an internal network")
        service_networks = {name: set(service.get("networks", {})) for name, service in services.items()}
        if service_networks.get("edge") != {"front", "private"}:
            fail("the edge must bridge the loopback-published front and isolated private networks")
        if service_networks.get("gateway") != {"private"}:
            fail("the gateway must remain isolated from the host-facing front network")
        for name in {"mock-node", "redis", "redpanda", "usage-topic-init"}:
            if service_networks.get(name) != {"private"}:
                fail(f"{name} must attach only to the isolated private network")

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
        print("PASS: versioned images, isolated private services, localhost-only edge, protected secrets and TLS identities")
        return 0
    except (OSError, json.JSONDecodeError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
