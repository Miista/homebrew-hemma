#!/bin/sh
# Stand-in for the `authelia` binary: hemma's apply only runs `authelia config validate`
# inside the auth container (then restarts it). Anything else is refused so a wrong call is visible.
if [ "$1 $2" = "config validate" ]; then
  echo "Configuration parsed and loaded successfully without errors. (fixture stub)"
  exit 0
fi
echo "authelia-stub: unsupported arguments: $*" >&2
exit 1
