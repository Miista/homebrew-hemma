#!/usr/bin/env bash
# Read-only: did the last Deploy/Apply on the fixture leave the running stack matching the repo?
# Changes nothing — it only reads docker state and the repo. Run via `make fixture-verify`.
set -uo pipefail

ui=hemma-ui-fixture
bad=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
no()   { printf '  \033[31m✗\033[0m %s\n' "$*"; bad=1; }
note() { printf '  \033[2m· %s\033[0m\n' "$*"; }

docker inspect "$ui" >/dev/null 2>&1 || { echo "container $ui is not running (make fixture-redeploy)"; exit 2; }

echo "== Last deploy/apply (from the UI's log)"
last=$(docker logs "$ui" 2>&1 | grep -E 'run "(deploy|apply)"' | tail -1)
if [ -z "$last" ]; then note "none since the UI container started"
elif echo "$last" | grep -q 'exit=0'; then ok "$last"
else no "$last"; fi

echo; echo "== Containers (a restart shows as a fresh start time; Caddy reloads in place, so it won't change)"
for c in pihole caddy cloudflared "$ui"; do
  printf '  %-18s started %s\n' "$c" "$(docker inspect -f '{{.State.StartedAt}}' "$c" 2>/dev/null | cut -c1-19)"
done
note "now: $(date -u +%Y-%m-%dT%H:%M:%S) UTC"

echo; echo "== Repo vs origin (what Deploy pulls)"
repo=$(docker exec "$ui" sh -c 'cd /repo && echo "$(git rev-parse --short HEAD)|$(git rev-parse --short origin/main 2>/dev/null)|$(git status --short | wc -l | tr -d " ")"')
IFS='|' read -r head origin dirty <<<"$repo"
[ "$head" = "$origin" ] && ok "HEAD $head = origin/main $origin" || no "HEAD $head differs from origin/main $origin"
[ "${dirty:-0}" = "0" ] && ok "no uncommitted changes" || note "$dirty file(s) with unsaved changes — work in progress, not a mismatch (Deploy stays off until you Save or Discard)"

echo; echo "== Caddy: what is RUNNING vs what the files on disk say"
# 127.0.0.1, not localhost: busybox resolves localhost to ::1 and the admin API listens on IPv4 only.
running=$(docker exec caddy wget -qO- http://127.0.0.1:2019/config/ 2>/dev/null | tr ',' '\n' | grep -oE '"(host|dial)":\[?"[^"]+"' | sort -u)
ondisk=$(docker exec caddy caddy adapt --config /etc/caddy/Caddyfile --adapter caddyfile 2>/dev/null | tr ',' '\n' | grep -oE '"(host|dial)":\[?"[^"]+"' | sort -u)
if [ -z "$running" ]; then no "could not read Caddy's running config (admin API)"
elif [ "$running" = "$ondisk" ]; then ok "running config = on-disk Caddyfile ($(echo "$running" | grep -c host) site(s): $(echo "$running" | grep host | grep -oE '"[a-z0-9.-]+"$' | tr -d '"' | paste -sd' ' -))"
else no "running config differs from the files on disk (a reload is pending?)"; diff <(echo "$running") <(echo "$ondisk") | sed 's/^/      /'; fi
docker exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 && ok "the Caddyfile on disk validates" || no "the Caddyfile on disk does NOT validate"

echo
[ "$bad" = 0 ] && echo "Fixture matches the repo." || { echo "Something does not match — see ✗ above."; exit 1; }
