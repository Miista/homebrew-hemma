#!/bin/sh
# Adds the splitdns apt repository. Run as root:
#   curl -fsSL https://miista.github.io/homebrew-sd/setup.sh | sudo sh
set -eu
[ "$(id -u)" -eq 0 ] || { echo "run as root: curl -fsSL https://miista.github.io/homebrew-sd/setup.sh | sudo sh" >&2; exit 1; }
base=https://miista.github.io/homebrew-sd
curl -fsSL "$base/splitdns-archive-keyring.asc" -o /usr/share/keyrings/splitdns-archive-keyring.asc
echo "deb [signed-by=/usr/share/keyrings/splitdns-archive-keyring.asc] $base stable main" > /etc/apt/sources.list.d/splitdns.list
apt-get update -q
echo "splitdns repository added. Install with: sudo apt-get install splitdns"
