#!/usr/bin/env bash
# Build splitdns for each host's arch and deploy to /usr/local/bin/splitdns over ssh.
#
# Both hosts' /usr/local/bin/splitdns are user-writable (the file, not the dir), so no
# sudo is needed. We overwrite the file in place with `cat >` rather than mv:
# /usr/local/bin is root-owned, so a cross-device mv (from /tmp) can't unlink the
# target, but writing through the existing writable file works. Version is
# stamped from `git describe` so `splitdns version` is meaningful.
#
# Usage: ./deploy.sh [host ...]     (default: all hosts below)
#
# Kept portable to bash 3.2 (macOS default): no associative arrays.
set -euo pipefail

cd "$(dirname "$0")"

# "host:GOARCH" pairs. Add hosts here as the fleet grows.
FLEET="optiplex:amd64 pi:arm64"

archfor() {
    for pair in $FLEET; do
        [ "${pair%%:*}" = "$1" ] && { echo "${pair##*:}"; return 0; }
    done
    return 1
}

VER="$(git describe --tags --always 2>/dev/null || git rev-parse --short HEAD)"
LDFLAGS="-X splitdns/internal/cli.Version=$VER"

# Targets: args if given, else every host in FLEET.
if [ "$#" -gt 0 ]; then
    targets="$*"
else
    targets=""
    for pair in $FLEET; do targets="$targets ${pair%%:*}"; done
fi

for host in $targets; do
    arch="$(archfor "$host")" || {
        echo "!! unknown host '$host' (known: $FLEET)" >&2
        exit 2
    }
    echo "==> building $host ($arch) @ $VER"
    bin="$(mktemp)"
    GOOS=linux GOARCH="$arch" go build -ldflags "$LDFLAGS" -o "$bin" ./

    echo "==> deploying to $host:/usr/local/bin/splitdns"
    # Overwrite in place (dir is root-owned; the file is writable). Piping over
    # ssh avoids a temp file and the cross-device mv problem.
    ssh "$host" 'cat > /usr/local/bin/splitdns && chmod 755 /usr/local/bin/splitdns && echo "    $(hostname): $(splitdns version)"' < "$bin"
    rm -f "$bin"
done

echo "==> done"
