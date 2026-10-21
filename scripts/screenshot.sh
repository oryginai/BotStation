#!/usr/bin/env bash
# Saves a PNG screenshot of the running desktop (through the agent API).
#   scripts/screenshot.sh [file]     default: docs/images/desktop.png
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f .env ] || { echo "No .env yet: run ./setup.sh first" >&2; exit 1; }
get() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- || true; }
port=$(get PORT); port=${port:-8443}
out=${1:-docs/images/desktop.png}

curl -fsSk -m 20 -H "Authorization: Bearer $(get AGENT_TOKEN)" \
    "https://127.0.0.1:$port/agent/screenshot?format=png" -o "$out"
echo "Saved $out"
