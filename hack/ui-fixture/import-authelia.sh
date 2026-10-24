#!/usr/bin/env bash
# Give the fixture an Authelia configuration.yml built from a REAL one, WITHOUT copying anything secret: only the
# OIDC clients' redirect URIs and authorization policies survive (what hemma reads), moved to the fixture's domain
# (see importauthelia/main.go). The real file is read from STDIN and never touches disk:
#
#   ssh edge 'cat ~/docker/edge/authelia/data/config/configuration.yml' | make fixture-import-authelia SERVICES=<the real services.yaml>
#
# SERVICES is the real services.yaml of the same repo (its declared domains are what gets moved). Commits ONLY the
# new file, so unsaved changes in the fixture repo are left alone.
set -euo pipefail

services=${1:?usage: … | import-authelia.sh <the real services.yaml>}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
repo="$here/.work/repo"
bare="$here/.work/remote.git"
rel="edge/authelia/data/config/configuration.yml"     # the fixture's host is "edge"; hemma's convention is <host>/authelia/data/config/

[ -f "$services" ] || { echo "no such file: $services" >&2; exit 2; }
[ -d "$repo/.git" ] || { echo "no fixture at $repo — run ./setup.sh first" >&2; exit 2; }
[ -t 0 ] && { echo "the real configuration.yml must be piped in on stdin" >&2; exit 2; }

cd "$root"
go run ./hack/ui-fixture/importauthelia -services "$services" -dst "$repo/$rel"

# hemma's wiring check reads <host>/docker-compose.yml: it wants the authelia service's X_AUTHELIA_CONFIG to list the
# generated access-control file. A minimal stand-in with exactly that (NOT a copy of a real compose file, which
# describes a whole stack and may carry secrets). Written only if the fixture has none.
compose="edge/docker-compose.yml"
if [ ! -f "$repo/$compose" ]; then
  cat > "$repo/$compose" <<'EOF'
# Fixture only: a minimal stand-in for this host's docker-compose.yml, with just what hemma's Authelia wiring check reads.
services:
  authelia:
    container_name: authelia
    image: authelia/authelia:4.39
    environment:
      X_AUTHELIA_CONFIG: /config/configuration.yml,/config/hemma.access_control.generated.yml
    volumes:
      - ./authelia/data/config:/config
EOF
fi

cd "$repo"
git add -- "$rel" "$compose"
if git diff --cached --quiet -- "$rel" "$compose"; then echo "the fixture already has exactly these files"; exit 0; fi
git -c user.name=fixture -c user.email=fixture@example.com commit -q -m "fixture: authelia OIDC clients (non-secret shape, neutralized) and a minimal compose for the wiring check" -- "$rel" "$compose"
git -c "url.$bare.insteadOf=/remote.git" push -q origin main
echo "committed and pushed: $(git log -1 --format='%h %s')  (only $rel and $compose; your other changes are untouched)"
