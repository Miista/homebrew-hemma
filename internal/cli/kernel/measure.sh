#!/usr/bin/env bash
set -euo pipefail

URL="${1:?usage: measure.sh <url> [pin-ip]}"
# Optional: $2 = IP to pin via --resolve (A/B legs). When set, curl skips DNS,
# so the dns metric is meaningless and is omitted from the output.
PIN="${2:-}"
N="${3:-5}"
WARMUP="${4:-3}"

HOST=$(echo "$URL" | sed 's|https://||;s|http://||' | cut -d/ -f1)

# Force IPv4 (-4): split-horizon suppresses AAAA to ::, so curl would otherwise
# try the dead :: first and fall back to IPv4 — wasting a beat and reporting ::
# as the connected IP. The managed path is IPv4-only, so -4 is the correct path.
CURL_ARGS=(-4)
if [ -n "$PIN" ]; then
    CURL_ARGS+=(--resolve "$HOST:443:$PIN")
fi

RUNS_WORD="runs"; [ "$N" -eq 1 ] && RUNS_WORD="run"
echo "Measuring $URL ($N $RUNS_WORD)..."
# Warm up AND capture the IP curl actually connected to (%{remote_ip}) — with a
# pin this echoes $PIN, without one it is curl's natural resolution. With zero
# warmup there is no request to capture from, so the resolved line is skipped.
if [ "$WARMUP" -gt 0 ]; then
    echo "Warming up ($WARMUP)..."
    RESOLVED=""
    for _ in $(seq 1 "$WARMUP"); do
        RESOLVED=$(curl "${CURL_ARGS[@]}" -o /dev/null -s -w '%{remote_ip}' "$URL") || true
    done
    [ -z "$RESOLVED" ] && RESOLVED="unknown"
    echo "Resolved: $HOST -> $RESOLVED"
fi
[ -n "$PIN" ] && echo "(DNS excluded: IP pinned via --resolve)"
echo ""

declare -a dns connect tls ttfb total

for i in $(seq 1 $N); do
    read -r d c t s o < <(curl "${CURL_ARGS[@]}" -o /dev/null -s \
        -w "%{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total}" \
        "$URL") || true
    dns+=($d) connect+=($c) tls+=($t) ttfb+=($s) total+=($o)
    # awk (not bc — not installed everywhere, e.g. optiplex) converts s -> ms.
    # With a pin, dns is ~0 and omitted from the per-run line.
    if [ -n "$PIN" ]; then
        awk -v i="$i" -v c="$c" -v t="$t" -v s="$s" -v o="$o" 'BEGIN {
            printf "  run %d: connect=%.0fms tls=%.0fms ttfb=%.0fms total=%.0fms\n", \
                i, c*1000, t*1000, s*1000, o*1000
        }' /dev/null
    else
        awk -v i="$i" -v d="$d" -v c="$c" -v t="$t" -v s="$s" -v o="$o" 'BEGIN {
            printf "  run %d: dns=%.0fms connect=%.0fms tls=%.0fms ttfb=%.0fms total=%.0fms\n", \
                i, d*1000, c*1000, t*1000, s*1000, o*1000
        }' /dev/null
    fi
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

# Omit the dns row when pinned (no lookup happened).
[ -z "$PIN" ] && stats "dns" "${dns[@]}"
stats "connect" "${connect[@]}"
stats "tls"     "${tls[@]}"
stats "ttfb"    "${ttfb[@]}"
stats "total"   "${total[@]}"
