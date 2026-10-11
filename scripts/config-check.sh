#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [[ "${CORDOVA_ENV:-development}" != development ]]; then
  echo "The checked Compose stack is development-only; use a separate reviewed production configuration." >&2
  exit 1
fi
if [[ ! -f "$ROOT/.local/app-pepper" || ! -f "$ROOT/.local/redis-password" || ! -f "$ROOT/.local/mock-node-token" || ! -f "$ROOT/.local/kafka-admin-password" || ! -f "$ROOT/.local/kafka-app-password" || ! -f "$ROOT/.local/kafka-server-jaas.conf" || ! -f "$ROOT/.local/kafka-admin.properties" || ! -f "$ROOT/.local/dev-ca.crt" ]]; then
  bash "$ROOT/scripts/bootstrap-dev.sh"
fi
python3 "$ROOT/scripts/config-check.py"
