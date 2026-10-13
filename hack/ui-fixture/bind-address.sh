#!/usr/bin/env bash
# Prints the LAN address the fixture UI should bind to, for FIXTURE_BIND=auto:
#   Colima:          the VM's own address (a bridged VM is NOT reachable on the Mac's address)
#   Docker Desktop:  the Mac's LAN address (en0)
# Prints nothing and exits 1 when it cannot tell. Read-only.
set -u
ctx=$(docker context show 2>/dev/null || true)
case "$ctx" in
  colima*)
    profile=${ctx#colima}; profile=${profile#-}; profile=${profile:-default}
    addr=$(colima list 2>/dev/null | awk -v p="$profile" '$1 == p && $2 == "Running" { print $NF }')
    ;;
  *)
    addr=$(ipconfig getifaddr en0 2>/dev/null || true)
    ;;
esac
case "${addr:-}" in
  ""|"-") exit 1 ;;
  *) echo "$addr" ;;
esac
