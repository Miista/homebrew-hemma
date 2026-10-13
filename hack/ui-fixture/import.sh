#!/usr/bin/env bash
# Replace the fixture's services with a NEUTRALIZED copy of a real services.yaml (see importcfg/main.go: one
# fixture host, one throwaway domain, no ssh, no real addresses). Backs up first, so it can always be undone:
#   - a git tag marks the fixture repo as it was:   git reset --hard fixture-before-import-<time>
#   - unsaved changes in the fixture repo are stashed (git stash list / git stash pop)
# Run via `make fixture-import SRC=<path to a services.yaml>`. Touches only .work/ and the fixture's containers.
set -euo pipefail

src=${1:?usage: import.sh <path to a real services.yaml>}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
repo="$here/.work/repo"
bare="$here/.work/remote.git"

[ -f "$src" ] || { echo "no such file: $src" >&2; exit 2; }
[ -d "$repo/.git" ] || { echo "no fixture at $repo — run ./setup.sh first" >&2; exit 2; }

stamp=$(date +%Y%m%d-%H%M%S)
cd "$repo"
git tag "fixture-before-import-$stamp"
echo "backup: tag fixture-before-import-$stamp  (undo: git -C $repo reset --hard fixture-before-import-$stamp)"
if [ -n "$(git status --porcelain)" ]; then
  git stash push -u -q -m "fixture: before import $stamp"
  echo "backup: your unsaved fixture changes were stashed (git -C $repo stash list)"
fi

cd "$root"
go run ./hack/ui-fixture/importcfg -src "$src" -dst "$repo/services.yaml"

# The auth snippet the services file points at (a generic forward_auth block; @@AUTH_SERVICE@@ is filled in per host).
if grep -q '^    auth_snippet: shared/auth.caddy' "$repo/services.yaml" && [ ! -f "$repo/shared/auth.caddy" ]; then
  mkdir -p "$repo/shared"
  printf 'forward_auth @@AUTH_SERVICE@@ {\n\turi /api/authz/forward-auth\n\tcopy_headers Remote-User Remote-Groups Remote-Email Remote-Name\n}\n' > "$repo/shared/auth.caddy"
fi

# Regenerate everything from the new services.yaml with this checkout's hemma (advisories make doctor exit 1: fine).
go build -o "$root/hemma" .
"$root/hemma" -C "$repo" doctor --fix >/dev/null 2>&1 || true

cd "$repo"
git add -A
git -c user.name=fixture -c user.email=fixture@example.com commit -q -m "fixture: import services from $(basename "$src") (neutralized)"
# origin is /remote.git INSIDE the UI container; from here it is the bare repo on disk. Pushing through the
# 'origin' remote keeps origin/main in step, so the UI does not show the commit as unpushed.
git -c "url.$bare.insteadOf=/remote.git" push -q origin main
echo "committed and pushed: $(git log -1 --format='%h %s')"

# An auth service in the file needs its stub container (the compose 'auth' profile) or apply cannot restart it.
if grep -q '^    auth_service:' "$repo/services.yaml"; then
  # Compose reads .env, where FIXTURE_BIND may be "auto" (resolved by the Makefile, not by Compose): resolve it the same way.
  bind=$(sed -n 's/^FIXTURE_BIND=//p' "$here/.env" 2>/dev/null || true)
  [ "$bind" = auto ] && bind=$("$here/bind-address.sh" || true)
  (cd "$here" && FIXTURE_BIND="${bind:-127.0.0.1}" docker compose -f compose.yml --profile auth up -d authelia) \
    || { echo "could not start the authelia stub container: Deploy would fail at the auth step" >&2; exit 1; }
  echo "the authelia stub container is running"
fi

# Prove the generated Caddy config loads before anyone clicks Deploy.
if docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
  echo "caddy validates the generated config"
else
  echo "WARNING: caddy does NOT validate the generated config" >&2
  docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile 2>&1 | tail -5 >&2
  exit 1
fi
echo "done — open the UI and Deploy to make it live."
