# Bot Station MCP server

Lets an AI agent see and drive a Bot Station through MCP. It wraps the
computer's agent API ([docs/api.md](../docs/api.md)) as 26 tools:
screenshots, mouse, keyboard, windows, clipboard, apps, shell, and the
browser's pages by structure.

Python 3.11+, the official MCP SDK (`mcp` 2.x) and `httpx`. Serves
Streamable HTTP (the Docker setup) or stdio.

## With Docker Compose (recommended)

The repository's `compose.yaml` runs this server in its own container next to
the computer. There is nothing to configure here: from the repository root,

```bash
./setup.sh --up        # tokens in .env, build, start
scripts/connect.sh     # prints the connect command for your client
```

The endpoint is `http://127.0.0.1:8765/mcp`, with
`Authorization: Bearer $MCP_TOKEN`. Inside Compose the server reaches the
computer at `https://computer:8443` with `AGENT_TOKEN`, and pins the
computer's self-signed certificate by reading `cert.pem` from the shared
`bot-station-tls` volume (mounted read-only).

## Without Docker (stdio)

For a client that starts the server itself, or a computer somewhere else.

1. **Token.** The computer's `AGENT_TOKEN` (from `.env`).
2. **Certificate.** desktopd's certificate is self-signed unless you gave it
   a real one. Pin it by its fingerprint:

   ```bash
   docker logs bot-station 2>&1 | grep fingerprint
   # TLS certificate SHA-256 fingerprint: 383254BAE729…
   ```

   It stays the same while the `bot-station-tls` volume exists.
3. **Run it** (no install step needed with [uv](https://docs.astral.sh/uv/)):

   ```bash
   BOT_STATION_TOKEN=… BOT_STATION_CERT_SHA256=383254BAE729… \
     uv run --directory /path/to/bot-station/mcp bot-station-mcp
   ```

   Or `pip install ./mcp` and run `bot-station-mcp`.

Claude Code, for example:

```bash
claude mcp add bot-station \
  -e BOT_STATION_TOKEN=… -e BOT_STATION_CERT_SHA256=… \
  -- uv run --directory /path/to/bot-station/mcp bot-station-mcp
```

## Settings (environment)

Connecting to the computer:

| Variable | Default | |
|---|---|---|
| `BOT_STATION_TOKEN` | required | The computer's `AGENT_TOKEN` (`CONTROL_TOKEN` also works, but don't) |
| `BOT_STATION_URL` | `https://localhost:8443` | The computer's address |
| `BOT_STATION_CERT_SHA256` | unset | Pin the self-signed certificate (hex, colons allowed) |
| `BOT_STATION_CERT_PIN_FILE` | unset | Pin the certificate in this PEM file (what Compose uses) |
| `BOT_STATION_CA_CERT` | unset | Trust this CA file instead (a private CA) |
| `BOT_STATION_INSECURE` | `false` | `true` skips the certificate check. Local testing only |

With none of the certificate options, the system's trusted CAs are used:
right for a computer behind a real certificate (`TLS_CERT`/`TLS_KEY` or an
HTTPS proxy). TLS 1.3 is required either way.

Serving MCP:

| Variable / flag | Default | |
|---|---|---|
| `MCP_TRANSPORT` / `--transport` | `stdio` (`streamable-http` in Docker) | |
| `MCP_HOST` / `--host` | `127.0.0.1` (`0.0.0.0` in Docker) | HTTP: address to listen on |
| `MCP_PORT` / `--port` | `8765` | HTTP: port. The endpoint is `/mcp` |
| `MCP_TOKEN` | unset | HTTP: required bearer token (≥32 chars). Unset = no auth, with a warning |
| `MCP_ALLOWED_HOSTS` | `127.0.0.1:*,localhost:*,[::1]:*` | HTTP: accepted `Host`/`Origin` values (DNS-rebinding protection). `*` turns the check off |

Without `MCP_TOKEN`, whoever can reach the HTTP port controls the computer.

## Tools

Every action can return what the agent needs next in the same call, through
`observe`: `"screenshot"`, `"snapshot"` (browser page structure), `"both"` or
`"none"`. Before observing, the tool waits for the screen to stop changing
(up to 5 s), so the result shows the outcome rather than a half-drawn screen.

**Screen and input** (any app; default `observe="screenshot"`):

| Tool | |
|---|---|
| `computer_status` | Screen size, pointer, browser open, shell on/off |
| `screenshot` | Whole screen or a region; `scale` to save tokens |
| `click`, `move_mouse`, `drag`, `scroll` | Mouse, at screen coordinates |
| `type_text`, `press_keys` | Keyboard: any Unicode text; combos like `ctrl+l` |
| `run_actions` | Up to 100 steps in one call (the API's `/agent/actions`) |
| `wait_for_screen` | Until the screen is stable or changes; no screenshot cost |
| `list_windows`, `window_action` | Activate, close, minimize, maximize, move, resize |
| `launch_app` | Browser (optional URL), terminal, files |
| `get_clipboard`, `set_clipboard` | Desktop clipboard |
| `run_shell` | Root shell; only when the computer has `AGENT_EXEC=true` |

**Browser** (Helium, by page structure; default `observe="snapshot"`):

| Tool | |
|---|---|
| `browser_snapshot` | Interactive elements with refs, state and centre on screen; optional page text |
| `browser_navigate` | URL, back, forward, reload; waits for load, starts the browser if needed |
| `browser_click`, `browser_fill` | By ref, with the real mouse and keyboard; `fill` also picks `<select>` options |
| `browser_wait` | Load, network idle, or some text on the page |
| `browser_dialog` | Answer alert / confirm / prompt |
| `browser_tabs`, `browser_new_tab`, `browser_switch_tab`, `browser_close_tab` | Tabs |

A snapshot as the agent sees it:

```
Tab 5555…: "The Internet" https://the-internet.herokuapp.com/login
Page on screen at 4,96 size 1272x568; scrolled 0 of 641 px.
Elements: [ref] role "name" details @ centre x,y on screen
[1] label "Username" @ 390,420
[2] textbox "Username" value="tomsmith" focused @ 390,446
[4] textbox "Password" @ 390,509
[5] button "Login" @ 238,568
```

The server also sends the agent short usage instructions at startup: prefer
browser tools for web pages, coordinates are screen pixels, refs expire on
navigation. See `INSTRUCTIONS` in `bot_station_mcp/server.py`.

Errors from the computer (stale ref, element covered by a banner, browser not
running, shell disabled…) come back as tool errors with the server's message,
so the agent can correct itself.

## Measured (local, through MCP over stdio)

| Tool | Time |
|---|---|
| `screenshot` (full, JPEG 75) | ~20 ms |
| `browser_snapshot` | ~20 ms |
| `browser_navigate` + snapshot | ~1.5–3 s (page load) |
| `click` / `press_keys`, `observe="none"` | ~40–80 ms |
| `browser_fill` + submit + snapshot | ~1.5–4.5 s (waits for the results page to settle) |

## Testing

`tests/smoke_test.py` starts the server over stdio, the way a client does,
and calls every tool against a running computer. It opens pages and types in
the browser, so don't run it while someone is using the desktop.

```bash
cd mcp
BOT_STATION_TOKEN=… BOT_STATION_CERT_SHA256=… uv run python tests/smoke_test.py
# add --exec if the computer has AGENT_EXEC=true
```
