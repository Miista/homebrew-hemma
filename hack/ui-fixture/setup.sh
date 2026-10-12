#!/usr/bin/env bash
# Builds a SYNTHETIC homelab repo + local bare remote under .work/ for exercising
# hemma-ui end to end (apply, deploy) without touching any real host or data.
# Everything here is fake: RFC 5737-style addresses, a throwaway self-signed cert.
#
#   ./setup.sh              base fixture (pihole, caddy, cloudflared)
#   ./setup.sh --with-auth  OPTIONAL: also declare an auth service so apply/deploy
#                           exercise the auth-provider step. Start compose with
#                           `--profile auth` to match, or apply fails on the missing container.
set -euo pipefail

with_auth=0
case "${1:-}" in
  "") ;;
  --with-auth) with_auth=1 ;;
  *) echo "usage: $0 [--with-auth]" >&2; exit 2 ;;
esac

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work="${FIXTURE_WORK:-$here/.work}"            # override to build a second, isolated copy
HOST_IP="${FIXTURE_HOST_IP:-172.28.0.10}"       # the hemma-ui container's fixed IP (see compose.yml)

rm -rf "$work"
mkdir -p "$work"

(cd "$root" && go build -o "$work/hemma" .)
hemma="$work/hemma"

seed="$work/seed"
mkdir -p "$seed/pi/caddy/data/certs/example.com"
cd "$seed"
git init -q -b main .

cat > services.yaml <<EOF
hosts:
    pi:
        ip: $HOST_IP
domains:
    - example.com
defaults:
    dns_host: pi
    tunnel_dir: cloudflared
$([ "$with_auth" = 1 ] && echo "    auth_service: authelia")
services:
    demo:
        fqdn: demo.example.com
        host: pi
        backend: demo:80
$([ "$with_auth" = 1 ] && printf '    authelia:\n        fqdn: auth.example.com\n        host: pi\n        backend: authelia:9091\n')
EOF

# Throwaway self-signed cert at the path hemma's TLS snippet expects.
openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=*.example.com" \
  -keyout pi/caddy/data/certs/example.com/privkey.key \
  -out pi/caddy/data/certs/example.com/fullchain.cer 2>/dev/null

# The hand-maintained Caddyfile hemma requires (trusted_proxies import + generated import).
cat > pi/caddy/data/Caddyfile <<'EOF'
{
	auto_https off
	servers {
		import hemma.trusted_proxies.generated.caddy
	}
}
import hemma.generated.caddy
EOF

# A hand-written file for the UI's auth-snippet file picker to find (hemma's own outputs can't be picked).
cat > pi/caddy/data/auth.caddy <<'EOF'
# Fixture auth snippet (hand-written). A real one would hold the forward_auth directive.
EOF

mkdir -p pi/cloudflared
"$hemma" -C "$seed" doctor --fix >/dev/null 2>&1 || true

git add -A
git -c user.name=fixture -c user.email=fixture@example.com commit -q -m "fixture: initial synthetic homelab repo"

git init -q --bare "$work/remote.git"
git remote add origin "$work/remote.git"
git push -q -u origin main

# The UI container sees the remote at /remote.git, so the working clone points there.
git clone -q "$work/remote.git" "$work/repo"
git -C "$work/repo" remote set-url origin /remote.git
rm -rf "$seed"

echo "fixture ready in $work — now: docker compose -f $here/compose.yml up -d --build"
