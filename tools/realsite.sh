#!/bin/bash
# CPU and memory on a real, ad-carrying site (default: a news front page):
# load, settle, then scroll for ~10 s. Usage: tools/realsite.sh [url]
cd "$(dirname "$0")"
URL=${1:-https://www.theguardian.com/international}
C=bot-station
TOKEN=$(grep -E '^(AGENT_TOKEN|CONTROL_TOKEN)=' ../.env | head -1 | cut -d= -f2-)
API=https://127.0.0.1:${PORT:-8443}
api() { curl -sk -m 60 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
cpu() { docker exec $C awk '/usage_usec/ {print $2}' /sys/fs/cgroup/cpu.stat; }
anon() { docker exec $C awk '/^anon / {printf "%d", $2/1048576}' /sys/fs/cgroup/memory.stat; }
api -X POST $API/agent/browser/navigate -d "{\"url\":\"about:blank\"}" >/dev/null
a=$(cpu); t0=$(date +%s%N)
api -X POST $API/agent/browser/navigate -d "{\"url\":\"$URL\"}" >/dev/null
sleep 10
b=$(cpu); t1=$(date +%s%N)
echo "load + 10 s: $(( (b - a) * 100 / ((t1 - t0) / 1000) ))% cpu, anon $(anon) MiB"
a=$(cpu); t0=$(date +%s%N)
for i in $(seq 100); do api -X POST $API/agent/actions -d '{"actions":[{"type":"scroll","x":640,"y":400,"dy":3}]}' >/dev/null; sleep 0.1; done
b=$(cpu); t1=$(date +%s%N)
echo "scroll: $(( (b - a) * 100 / ((t1 - t0) / 1000) ))% cpu, anon $(anon) MiB, peak $(( $(docker exec $C cat /sys/fs/cgroup/memory.peak) / 1048576 )) MiB total"
