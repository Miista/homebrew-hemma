#!/usr/bin/env bash
# Give the fixture an Authelia configuration.yml built from a REAL one, WITHOUT copying anything secret: only the
# OIDC clients' redirect URIs and authorization policies survive (what hemma reads), moved to the fixture's domain
# (see importauthelia/main.go). The real file is read from STDIN and never touches disk:
#
#   ssh pi 'cat ~/docker/pi/authelia/data/config/configuration.yml' | make fixture-import-authelia SERVICES=<the real services.yaml>
#
# SERVICES is the real services.yaml of the same repo (its declared domains are what gets moved). Commits ONLY the
# new file, so unsaved changes in the fixture repo are left alone.
set -euo pipefail

services=${1:?usage: … | import-authelia.sh <the real services.yaml>}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
repo="$here/.work/repo"
bare="$here/.work/remote.git"
rel="pi/authelia/data/config/configuration.yml"     # the fixture's host is "pi"; hemma's convention is <host>/authelia/data/config/

[ -f "$services" ] || { echo "no such file: $services" >&2; exit 2; }
[ -d "$repo/.git" ] || { echo "no fixture at $repo — run ./setup.sh first" >&2; exit 2; }
[ -t 0 ] && { echo "the real configuration.yml must be piped in on stdin" >&2; exit 2; }

cd "$root"
go run ./hack/ui-fixture/importauthelia -services "$services" -dst "$repo/$rel"

cd "$repo"
git add -- "$rel"
if git diff --cached --quiet -- "$rel"; then echo "the fixture already has exactly this file"; exit 0; fi
git -c user.name=fixture -c user.email=fixture@example.com commit -q -m "fixture: authelia OIDC clients (non-secret shape, neutralized)" -- "$rel"
git -c "url.$bare.insteadOf=/remote.git" push -q origin main
echo "committed and pushed: $(git log -1 --format='%h %s')  (only $rel; your other changes are untouched)"
