#!/usr/bin/env bash
set -euo pipefail

URL="${1:-https://happier.example.com}"
N=5

HOST=$(echo "$URL" | sed 's|https://||;s|http://||' | cut -d/ -f1)

# Force IPv4 (-4): split-horizon suppresses AAAA to ::, so curl would otherwise
# try the dead :: first and fall back to IPv4 — wasting a beat and reporting ::
# as the connected IP. The managed path is IPv4-only, so -4 is the correct path.
echo "Measuring $URL ($N runs)..."
echo "Warming up..."
# Warm up AND capture the IP curl actually connected to (%{remote_ip}). Using
# curl's own answer — not a separate dig/getent — guarantees the displayed IP is
# the one we measured.
RESOLVED=""
for _ in 1 2 3; do
    RESOLVED=$(curl -4 -o /dev/null -s -w '%{remote_ip}' "$URL") || true
done
[ -z "$RESOLVED" ] && RESOLVED="unknown"
echo "Resolved: $HOST -> $RESOLVED"
echo ""

declare -a dns connect tls ttfb total

for i in $(seq 1 $N); do
    read -r d c t s o < <(curl -4 -o /dev/null -s \
        -w "%{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total}" \
        "$URL") || true
    dns+=($d) connect+=($c) tls+=($t) ttfb+=($s) total+=($o)
    # awk (not bc — not installed everywhere, e.g. optiplex) converts s -> ms.
    awk -v i="$i" -v d="$d" -v c="$c" -v t="$t" -v s="$s" -v o="$o" 'BEGIN {
        printf "  run %d: dns=%.0fms connect=%.0fms tls=%.0fms ttfb=%.0fms total=%.0fms\n", \
            i, d*1000, c*1000, t*1000, s*1000, o*1000
    }' /dev/null
done

echo ""

stats() {
    local label=$1; shift
    local values=("$@")
    awk -v label="$label" -v n="${#values[@]}" '
    BEGIN {
        split("'"${values[*]}"'", a, " ")
        sum=0; for(i=1;i<=n;i++) sum+=a[i]*1000
        mean=sum/n
        sq=0; for(i=1;i<=n;i++) sq+=(a[i]*1000-mean)^2
        sd=sqrt(sq/n)
        min=a[1]*1000; max=a[1]*1000
        for(i=2;i<=n;i++) { if(a[i]*1000<min) min=a[i]*1000; if(a[i]*1000>max) max=a[i]*1000 }
        printf "%-10s  mean=%5.0fms  sd=%4.0fms  min=%5.0fms  max=%5.0fms\n", label, mean, sd, min, max
    }' /dev/null
}

stats "dns"     "${dns[@]}"
stats "connect" "${connect[@]}"
stats "tls"     "${tls[@]}"
stats "ttfb"    "${ttfb[@]}"
stats "total"   "${total[@]}"
