#!/usr/bin/env bash
# Prints how to connect an MCP client (Claude Code, Codex, Claude Desktop,
# Cursor, VS Code...) to this Bot Station, with the token from .env filled in.
#   scripts/connect.sh
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f .env ] || { echo "No .env yet: run ./setup.sh first" >&2; exit 1; }
get() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- || true; }
token=$(get MCP_TOKEN)
port=$(get MCP_PORT); port=${port:-8765}
url="http://127.0.0.1:$port/mcp"

# The auth part of each snippet (nothing when MCP_TOKEN is empty)
claude_auth="" codex_auth="" json_auth="" remote_auth=""
if [ -n "$token" ]; then
    claude_auth=" \\
    --header \"Authorization: Bearer $token\""
    codex_auth="
  bearer_token_env_var = \"BOT_STATION_MCP_TOKEN\"

  and in the shell that runs codex:
  export BOT_STATION_MCP_TOKEN=$token"
    json_auth=",
        \"headers\": { \"Authorization\": \"Bearer $token\" }"
    remote_auth=", \"--header\", \"Authorization: Bearer $token\""
else
    echo "Note: MCP_TOKEN is empty, so the MCP port has no authentication."
fi

cat <<EOF
MCP endpoint: $url   (Streamable HTTP)

Claude Code:
  claude mcp add --transport http bot-station $url$claude_auth

Codex (~/.codex/config.toml):
  [mcp_servers.bot-station]
  url = "$url"$codex_auth

Cursor / VS Code / other clients with a JSON config:
  {
    "mcpServers": {
      "bot-station": {
        "type": "http",
        "url": "$url"$json_auth
      }
    }
  }

Claude Desktop (bridged through mcp-remote; needs Node.js):
  "bot-station": {
    "command": "npx",
    "args": ["-y", "mcp-remote", "$url"$remote_auth]
  }

Watch the desktop live in your browser: scripts/view.sh
EOF
