#!/usr/bin/env bash
# Bot Station setup. Safe to run again: it only fills in what's missing.
#
#   ./setup.sh          create .env (from .env.example) with fresh tokens
#   ./setup.sh --up     ...then build and start the containers, and wait
#                       until the computer and the MCP server are ready
#
# At the end it prints how to connect an agent (scripts/connect.sh).
set -euo pipefail
cd "$(dirname "$0")"

up=false
case "${1:-}" in
    --up) up=true ;;
    "") ;;
    -h|--help) sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1 (try --help)" >&2; exit 2 ;;
esac

token() {
    if command -v openssl >/dev/null; then openssl rand -hex 32
    else head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}

# 1. .env from the documented template
if [ ! -f .env ]; then
    cp .env.example .env
    echo "Created .env from .env.example"
fi
chmod 600 .env

# 2. Tokens: generate any that are empty or missing; never replace one
for key in CONTROL_TOKEN AGENT_TOKEN MCP_TOKEN; do
    current=$(grep -E "^$key=" .env | tail -1 | cut -d= -f2- || true)
    if [ -z "$current" ]; then
        value=$(token)
        if grep -qE "^$key=" .env; then
            sed -i.bak "s|^$key=.*|$key=$value|" .env && rm -f .env.bak
        else
            printf '%s=%s\n' "$key" "$value" >> .env
        fi
        echo "Generated $key"
    fi
done

# 3. Optionally start everything
if $up; then
    command -v docker >/dev/null || { echo "Docker is not installed: https://docs.docker.com/get-docker/" >&2; exit 1; }
    echo "Building and starting (the first build takes a few minutes)..."
    docker compose up -d --build --wait
    echo "Bot Station is running."
fi

echo
scripts/connect.sh
