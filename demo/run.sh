#!/usr/bin/env bash
# Runs demo/pivot_test.py: starts the test sites next to a running Bot
# Station, then drives it through the MCP server over stdio.
set -euo pipefail
cd "$(dirname "$0")/.."
compose=(docker compose -f compose.yaml -f demo/compose.demo.yaml)
"${compose[@]}" up -d --wait --no-build demo computer keeper 2>/dev/null
get() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- || true; }
cert=$(mktemp); trap 'rm -f "$cert"' EXIT
"${compose[@]}" exec -T computer cat /var/lib/desktopd/cert.pem > "$cert"
port=$(get PORT)
token=$(get AGENT_TOKEN)
cd mcp
BOT_STATION_URL=https://127.0.0.1:${port:-8443} BOT_STATION_TOKEN=$token BOT_STATION_CERT_PIN_FILE=$cert \
    uv run python ../demo/pivot_test.py
