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
set -- -listen "${CORDOVA_LISTEN:-127.0.0.1:8080}"
web_dir=${CORDOVA_WEB_DIR:-}
if [ -z "$web_dir" ] && [ -f frontend/build/index.html ]; then
    web_dir=frontend/build
fi
if [ -n "$web_dir" ]; then
    set -- "$@" -web-dir "$web_dir"
fi
exec bin/cordova-gateway "$@"
