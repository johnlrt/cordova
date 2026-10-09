#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOCAL="$ROOT/.local"
umask 077
mkdir -p "$LOCAL"
chmod 700 "$LOCAL"

command -v openssl >/dev/null 2>&1 || { echo "openssl is required to create local development credentials" >&2; exit 1; }

write_secret() {
  local name="$1"
  local path="$LOCAL/$name"
  if [[ -L "$path" ]]; then echo "refusing symlink: $path" >&2; exit 1; fi
  if [[ -e "$path" ]]; then
    [[ -f "$path" && -s "$path" ]] || { echo "existing secret is not a nonempty regular file: $path" >&2; exit 1; }
    chmod 600 "$path"
    return
  fi
  openssl rand -hex 32 > "$path"
  chmod 600 "$path"
}

make_ca() {
  local key="$LOCAL/dev-ca.key" cert="$LOCAL/dev-ca.crt"
  if [[ -e "$key" || -e "$cert" ]]; then
    [[ -f "$key" && -f "$cert" && ! -L "$key" && ! -L "$cert" ]] || { echo "incomplete local CA pair; inspect .local before retrying" >&2; exit 1; }
    return
  fi
  openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 3650 \
    -keyout "$key" -out "$cert" -subj "/CN=Cordova local development CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"
  chmod 600 "$key"
  chmod 644 "$cert"
}

make_leaf() {
  local name="$1" subject="$2" san="$3"
  local key="$LOCAL/$name.key" cert="$LOCAL/$name.crt"
  if [[ -e "$key" || -e "$cert" ]]; then
    [[ -f "$key" && -f "$cert" && ! -L "$key" && ! -L "$cert" ]] || { echo "incomplete $name certificate pair; inspect .local before retrying" >&2; exit 1; }
    return
  fi
  local csr ext
  csr="$(mktemp "$LOCAL/$name.XXXXXX.csr")"
  ext="$(mktemp "$LOCAL/$name.XXXXXX.ext")"
  trap 'rm -f "${csr:-}" "${ext:-}"' RETURN
  printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature,keyEncipherment\nbasicConstraints=critical,CA:FALSE\n' "$san" > "$ext"
  openssl req -new -newkey rsa:2048 -sha256 -nodes -keyout "$key" -out "$csr" -subj "$subject"
  openssl x509 -req -in "$csr" -CA "$LOCAL/dev-ca.crt" -CAkey "$LOCAL/dev-ca.key" \
    -CAcreateserial -out "$cert" -days 825 -sha256 -extfile "$ext"
  chmod 600 "$key"
  chmod 644 "$cert"
  rm -f "$csr" "$ext"
  trap - RETURN
}

write_secret app-pepper
write_secret redis-password
write_secret mock-node-token
make_ca
make_leaf edge "/CN=localhost" "DNS:localhost,IP:127.0.0.1"
make_leaf mock-node "/CN=mock-node" "DNS:mock-node"

echo "Local M1 secrets and TLS certificates are ready under .local/. Keep dev-ca.key private."
