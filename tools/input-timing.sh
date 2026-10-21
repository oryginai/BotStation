#!/bin/bash
# How long agent input takes: one click across the screen, typing 50
# characters. Shows the cost of AGENT_HUMAN_INPUT. Usage: tools/input-timing.sh
cd "$(dirname "$0")"
TOKEN=$(grep -E '^(AGENT_TOKEN|CONTROL_TOKEN)=' ../.env | head -1 | cut -d= -f2-)
API=https://127.0.0.1:${PORT:-8443}
api() { curl -sk -m 60 -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }
api -X POST $API/agent/browser/navigate -d '{"url":"about:blank"}' >/dev/null
ms() { local t0=$(date +%s%N); api -X POST $API/agent/actions -d "$1" >/dev/null; echo $(( ($(date +%s%N) - t0) / 1000000 )); }
c=0; t=0
for i in 1 2 3 4 5; do
  api -X POST $API/agent/actions -d '{"actions":[{"type":"move","x":100,"y":150}]}' >/dev/null
  c=$((c + $(ms '{"actions":[{"type":"click","x":1100,"y":600}]}')))
  t=$((t + $(ms '{"actions":[{"type":"type","text":"the quick brown fox jumps over the lazy dog 12345"}]}')))
done
echo "click across the screen: $((c / 5)) ms, typing 50 chars: $((t / 5)) ms (average of 5, incl. ~5 ms HTTP)"
