#!/usr/bin/env sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"
if [ ! -x bin/cordova-gateway ]; then
    mkdir -p bin
    (cd server && go build -trimpath -o ../bin/cordova-gateway ./cmd/gateway)
fi
if [ -z "${CORDOVA_API_KEY:-}" ] && [ ! -f .local/api-key.txt ]; then
    bin/cordova-gateway -init-key
fi
exec bin/cordova-gateway -listen "${CORDOVA_LISTEN:-127.0.0.1:8080}"
