# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex, Cursor...) that were
given this repository. Two jobs come up: **setting Bot Station up for the
user**, and **working on its code**.

## Setting it up and connecting the MCP server

Run these from the repository root. Ask the user before installing Docker
or changing anything outside this directory.

1. **Check Docker.** `docker compose version` must print v2.20 or newer, and
   `docker info` must work without errors. If either fails, stop and tell the
   user; don't install Docker for them unasked.

2. **Configure and start.**

   ```bash
   ./setup.sh --up
   ```

   This creates `.env` with three random tokens (it keeps any that already
   exist), builds both images and waits until they are healthy. The first
   build takes ~3–10 minutes: run it with a long timeout, or in the
   background and poll `docker compose ps`.

3. **Verify.**

   ```bash
   docker compose ps   # computer: healthy; mcp: running
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8765/mcp   # 401: up, auth on
   ```

   If something is wrong, look at `docker compose logs computer` and
   `docker compose logs mcp`.

4. **Connect the MCP server.** `scripts/connect.sh` prints the exact snippet
   for each client, with the token from `.env` filled in.
   - **Claude Code**: run the printed `claude mcp add --transport http bot-station ...` command.
   - **Codex**: add the printed `[mcp_servers.bot-station]` block to
     `~/.codex/config.toml`, and have the user export `BOT_STATION_MCP_TOKEN`
     (ask before editing their shell profile).
   - **Another client**: use the JSON snippet.

   New MCP servers usually show up only in a new session. Tell the user to
   restart the client (in Claude Code, `/mcp` shows the server's status).

5. **Report back.** Give the user the endpoint (`http://127.0.0.1:8765/mcp`),
   tell them `scripts/view.sh` prints a link to watch the desktop live, and
   tell them where the tokens are (`.env`). Don't paste tokens into chat
   unless they ask.

Rules:
- Never commit `.env` or print it in full. It holds every credential.
- Keep `BIND_ADDRESS=127.0.0.1` unless the user explicitly asks to expose it.
- Leave `AGENT_EXEC=false` (no root shell for agents) unless the user asks.
- `docker compose down -v` deletes the user's files and browser logins. Only
  run it if the user asked for exactly that.

## Using it (once the tools are connected)

The MCP server sends its own usage instructions when you connect. In short:
prefer the `browser_*` tools for web pages (`browser_snapshot` → act by
`ref`); use screenshots with `click` / `type_text` / `press_keys` for
everything else; coordinates are screen pixels on a 1280x720 screen. If a
page shows a CAPTCHA or another human check, ask the user to solve it in the
live view (`scripts/view.sh`) instead of trying to get around it.

## Working on the code

| Part | Where | Check with |
|---|---|---|
| desktopd: streaming, control + agent APIs, browser over CDP (Go) | `computer/server/` | `cd computer/server && go vet ./... && go test ./...` |
| MCP server (Python 3.11+, `mcp` SDK 2.x) | `mcp/bot_station_mcp/` | `cd mcp && uvx ruff check . && uv run python -c "import bot_station_mcp.server"` |
| Desktop image | `computer/Dockerfile`, `entrypoint.sh`, `theme/`, `xfconf/`, `helium/` | `docker compose build computer` |
| Compose | `compose.yaml` | `docker compose config -q` |
| End to end | `mcp/tests/smoke_test.py` | see [mcp/README.md](mcp/README.md#testing). It drives the real desktop: not while someone is using it |

Read [docs/architecture.md](docs/architecture.md) before larger changes. Its
"Gotchas" section lists traps that cost real time (compositors, Chromium
profile locks, Xvfb resets, Helium's 4 px page inset...).

Conventions:
- Match the surrounding style: short comments that explain *why*, plain
  names, no new dependencies without a reason.
- A new agent API endpoint needs: the handler in `computer/server/`, its
  entry in [docs/api.md](docs/api.md), a tool in `mcp/bot_station_mcp/server.py`
  if agents should use it, and a line in `mcp/tests/smoke_test.py`.
- A new setting needs: a default in the Go config (`main.go`) or the
  entrypoint, and a commented entry in `.env.example`.
