#!/usr/bin/env bash
# Gives every certificate the fixture's Caddy config loads a throwaway self-signed one, so `caddy validate` (the first step of
# apply) passes. It reads the `tls <cert> <key>` lines hemma generated in each host's caddy/data — /etc/caddy in the container
# is that folder — and creates only what is missing, valid for a year. Run by setup.sh and import.sh before they commit:
#   ./certs.sh <fixture repo>
set -euo pipefail

repo=${1:?usage: certs.sh <fixture repo>}
for data in "$repo"/*/caddy/data; do
  [ -d "$data" ] || continue
  grep -rhoE '^[[:space:]]*tls /etc/caddy/[^ ]+ /etc/caddy/[^ ]+' "$data" 2>/dev/null | sort -u |
  while read -r _ cert key; do
    cert="$data/${cert#/etc/caddy/}" key="$data/${key#/etc/caddy/}"
    [ -f "$cert" ] && [ -f "$key" ] && continue
    domain=$(basename "$(dirname "$cert")")
    mkdir -p "$(dirname "$cert")" "$(dirname "$key")"
    openssl req -x509 -newkey rsa:2048 -nodes -days 365 -subj "/CN=*.$domain" -keyout "$key" -out "$cert" 2>/dev/null
    echo "throwaway cert for *.$domain: ${cert#"$repo"/}"
  done
done
