#!/usr/bin/env bash
# Opens the live view: turns streaming on and prints a one-time link to
# watch (and use) the desktop in your browser.
#   scripts/view.sh              link with mouse and keyboard control
#   scripts/view.sh --view-only  link that can only watch
#   scripts/view.sh --off        turn streaming off, disconnect all viewers
# The link works once and expires after VIEWER_TOKEN_TTL (60 s): open it
# right away. The certificate is self-signed, so the browser asks you to
# accept it the first time.
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f .env ] || { echo "No .env yet: run ./setup.sh first" >&2; exit 1; }
get() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- || true; }
token=$(get CONTROL_TOKEN)
port=$(get PORT); port=${port:-8443}
api="https://127.0.0.1:$port"
call() { curl -fsSk -m 10 -X POST -H "Authorization: Bearer $token" -H 'Content-Type: application/json' "$@"; }

view_only=false
case "${1:-}" in
    --view-only) view_only=true ;;
    --off) call "$api/api/disable" >/dev/null && echo "Streaming is off."; exit 0 ;;
    "") ;;
    *) echo "unknown option: $1 (--view-only or --off)" >&2; exit 2 ;;
esac

call "$api/api/enable" >/dev/null ||
    { echo "Can't reach Bot Station at $api (is it running? docker compose ps)" >&2; exit 1; }
res=$(call -d "{\"view_only\":$view_only}" "$api/api/sessions")
link=$(printf '%s' "$res" | sed -n 's/.*"view_url":"\([^"]*\)".*/\1/p')
[ -n "$link" ] || { echo "Unexpected answer: $res" >&2; exit 1; }
echo "Open within 60 s (works once):"
echo "  $link"
