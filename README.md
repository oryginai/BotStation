<div align="center">

# Bot Station

**A Linux desktop for AI agents, in one container. Connect it over MCP; watch it live from a browser.**

[Quick start](#quick-start) · [Connect an agent](#connect-an-agent) · [Watch it live](#watch-it-live) · [Docs](#documentation)

</div>

Bot Station gives an AI agent a real computer to work in: a desktop with a
browser, a terminal and a file manager. The agent drives it through
**26 MCP tools**: screenshots, mouse, keyboard, windows, clipboard, and web
pages read by their structure instead of by pixels. You can watch what it does
in your own browser, and take over the mouse when it needs a hand.

- **One command to run.** `./setup.sh --up` generates tokens, builds two containers and starts them.
- **Works with any MCP client.** Claude Code, Codex, Cursor, VS Code, Claude Desktop: it is a standard Streamable HTTP endpoint.
- **Built for agents.** Page snapshots list every interactive element with a ref, so `browser_click(ref=5)` replaces guessing coordinates. Every action can return the next screenshot or snapshot in the same call.
- **Live view, no VNC.** A purpose-built streaming server (`desktopd`, Go) sends JPEG tiles over a WebSocket. It costs nothing while the screen is idle.
- **Locked down by default.** TLS 1.3, separate tokens for you and the agent, ports bound to localhost, a sandboxed browser, and a non-root, read-only MCP container.

<p align="center">
  <img src="docs/images/architecture.svg" alt="Architecture: AI agents connect over MCP to the mcp container, which drives the computer container's desktopd through the agent API. desktopd captures the Xvfb display and injects input; people watch through the live view." width="900">
</p>

## Quick start

You need **Docker** with **Compose v2.20+** (Docker Desktop, or Docker Engine
on Linux). The computer runs as x86_64; on Apple Silicon it runs emulated
(slower, but it works).

```bash
git clone https://github.com/oryginai/BotStation.git
cd BotStation
./setup.sh --up
```

That's it. The first build downloads Ubuntu and the Helium browser and takes
a few minutes; later starts take about a second. When it finishes, `setup.sh`
prints the command that connects your agent.

<details>
<summary>Doing it by hand instead</summary>

```bash
cp .env.example .env
# set CONTROL_TOKEN, AGENT_TOKEN and MCP_TOKEN, each to the output of: openssl rand -hex 32
docker compose up -d --build --wait
scripts/connect.sh
```

</details>

Two containers are now running, both on `127.0.0.1` only:

| Service | Address | What |
|---|---|---|
| `mcp` | `http://127.0.0.1:8765/mcp` | The MCP server your agent connects to |
| `computer` | `https://127.0.0.1:8443` | The desktop: live view, control API, agent API |

## Connect an agent

`scripts/connect.sh` prints these snippets with your token already filled in.
The token is `MCP_TOKEN` from `.env`.

**Claude Code**

```bash
claude mcp add --transport http bot-station http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer <MCP_TOKEN>"
```

**Codex**: add this to `~/.codex/config.toml`, and `export BOT_STATION_MCP_TOKEN=<MCP_TOKEN>` in the shell that runs `codex`:

```toml
[mcp_servers.bot-station]
url = "http://127.0.0.1:8765/mcp"
bearer_token_env_var = "BOT_STATION_MCP_TOKEN"
```

**Cursor, VS Code and other JSON-configured clients**

```json
{
  "mcpServers": {
    "bot-station": {
      "type": "http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": { "Authorization": "Bearer <MCP_TOKEN>" }
    }
  }
}
```

**Claude Desktop** (bridged through [`mcp-remote`](https://www.npmjs.com/package/mcp-remote); needs Node.js)

```json
"bot-station": {
  "command": "npx",
  "args": ["-y", "mcp-remote", "http://127.0.0.1:8765/mcp", "--header", "Authorization: Bearer <MCP_TOKEN>"]
}
```

Then ask your agent something like: *"Use bot-station to open
news.ycombinator.com and tell me the top three stories."*

### Let your agent set it up

Give an agent the repository link and ask:

> Set up Bot Station from https://github.com/oryginai/BotStation and connect its MCP server to yourself.

[`AGENTS.md`](AGENTS.md) (also read by Claude Code through `CLAUDE.md`) walks
the agent through cloning, `./setup.sh --up`, checking that both services
are up, and registering the MCP server. Most clients need a restart or a new
session before new MCP tools show up.

## Watch it live

```bash
scripts/view.sh              # link with mouse and keyboard control
scripts/view.sh --view-only  # link that can only watch
scripts/view.sh --off        # stop streaming, disconnect everyone
```

The script prints a link like `https://127.0.0.1:8443/view#token=…`. Open it
within 60 seconds; each link works once. The certificate is self-signed, so
the browser asks you to accept it the first time.

The agent and you share one mouse and keyboard. When a site shows a human
check (a CAPTCHA, a Cloudflare challenge), the agent is told to ask you to
solve it in the live view rather than try to get around it.

## What the agent gets

| | Tools |
|---|---|
| **Screen and input** (any app) | `screenshot`, `click`, `move_mouse`, `drag`, `scroll`, `type_text`, `press_keys`, `run_actions` (up to 100 steps per call), `wait_for_screen` |
| **Desktop** | `computer_status`, `list_windows`, `window_action`, `launch_app`, `get_clipboard`, `set_clipboard`, `run_shell` (off unless `AGENT_EXEC=true`) |
| **Browser by structure** | `browser_snapshot`, `browser_navigate`, `browser_click`, `browser_fill`, `browser_wait`, `browser_dialog`, `browser_tabs`, `browser_new_tab`, `browser_switch_tab`, `browser_close_tab` |

A page snapshot, as the agent sees it:

```
Tab 5555…: "The Internet" https://the-internet.herokuapp.com/login
Page on screen at 4,96 size 1272x568; scrolled 0 of 641 px.
Elements: [ref] role "name" details @ centre x,y on screen
[1] label "Username" @ 390,420
[2] textbox "Username" value="tomsmith" focused @ 390,446
[4] textbox "Password" @ 390,509
[5] button "Login" @ 238,568
```

Before it observes, every action waits for the screen to stop changing, so
results show the outcome rather than a half-drawn page. Typical timings: a
screenshot or a snapshot takes ~20 ms, a click ~50 ms, and a navigation the
page's load time. More in [mcp/README.md](mcp/README.md).

## Configuration

Everything lives in `.env`; [`.env.example`](.env.example) documents every
option. The ones you're most likely to change:

| Variable | Default | |
|---|---|---|
| `CPU_LIMIT` / `MEMORY_LIMIT` | `2` / `2g` | Container limits. The browser is the big user: plan ~500 MB per busy tab |
| `TZ`, `BROWSER_LANG` | UTC, `en-US,en` | Timezone and languages websites see. Match them to your IP's region |
| `BROWSER_PROXY` | unset | `scheme://host:port` proxy for the browser |
| `BROWSER_ADBLOCK` | `false` | Helium's built-in uBlock Origin. Turn it on for ad-heavy browsing |
| `AGENT_EXEC` | `false` | Allow the `run_shell` tool (a root shell in the container) |
| `AGENT_HUMAN_INPUT` | `true` | Human-paced mouse paths and typing. `false` is instant |
| `BIND_ADDRESS` | `127.0.0.1` | `0.0.0.0` exposes both ports on every interface. Read [Security](#security) first |
| `PORT` / `MCP_PORT` | `8443` / `8765` | Host ports |

After changing `.env`, run `docker compose up -d` again.

## Day-to-day

```bash
docker compose ps                 # status
docker compose logs -f            # logs from both services
docker compose restart computer   # restart the desktop (files and browser profile persist)
docker compose down               # stop; data stays in volumes
docker compose down -v            # stop and delete all data (files, browser profile, certificate)
git pull && docker compose up -d --build   # update
scripts/screenshot.sh             # save a PNG of the desktop to docs/images/desktop.png
```

Files in `/root` and the browser profile (logins, history) live in Docker
volumes, so they survive restarts, rebuilds and updates. Packages installed at
runtime with `apt` don't; add them to `computer/Dockerfile` instead.

## Security

- Both ports listen on **127.0.0.1** by default. To reach Bot Station from
  another machine, prefer an SSH tunnel or a reverse proxy with TLS and auth.
  If you do set `BIND_ADDRESS=0.0.0.0`, also add your hostname to
  `MCP_ALLOWED_HOSTS`, and put a firewall in front.
- **Three tokens, three roles.** `CONTROL_TOKEN` is yours: it turns the live
  view on and off. `AGENT_TOKEN` is held only by the MCP container.
  `MCP_TOKEN` is what agents present. An agent never sees the other two.
- **Anyone with `MCP_TOKEN` controls the desktop**, including whatever the
  browser is logged into. Treat it like a password.
- The browser runs as an unprivileged user inside Chromium's sandbox. The
  container runs with `no-new-privileges` and a seccomp profile.
- `run_shell` is off unless you set `AGENT_EXEC=true`.

Details in [docs/architecture.md](docs/architecture.md#security). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `AGENT_TOKEN is not set in .env` | Run `./setup.sh` |
| `mcp` stays in "Created" / "Waiting" | The computer isn't healthy yet: `docker compose logs computer` |
| The agent gets HTTP 401 | The client isn't sending `Authorization: Bearer <MCP_TOKEN>`, or the token is outdated. Run `scripts/connect.sh` again |
| HTTP 421 "Invalid Host header" | You reach the MCP port by a name other than localhost: add it to `MCP_ALLOWED_HOSTS` |
| The browser won't start, "profile in use" | Run `docker compose restart computer`; stale locks are cleared at start |
| Sites say "Access denied" | Many sites block datacenter IPs. Use `BROWSER_PROXY` with an ISP IP, and set `TZ`/`BROWSER_LANG` to match it |
| `storage-opt is supported only for…` | `compose.disk-limit.yaml` needs XFS with `pquota`. Leave it out |
| Very slow on a Mac with Apple Silicon | Expected: the desktop runs emulated as x86_64 |

## Project structure

```
BotStation/
├── compose.yaml            # both services; start here
├── setup.sh                # .env with fresh tokens (+ --up to start)
├── .env.example            # every option, documented
├── scripts/
│   ├── connect.sh          # MCP connect snippets for each client
│   ├── view.sh             # one-time live-view link
│   └── screenshot.sh       # PNG of the desktop
├── computer/               # the desktop image
│   ├── Dockerfile          # Go build → Helium (signature-checked) → Ubuntu + XFCE
│   ├── entrypoint.sh       # starts Xvfb, window manager, panel, desktopd
│   ├── server/             # desktopd (Go): streaming, control + agent APIs, browser over CDP
│   ├── helium/             # browser launcher and policies
│   ├── theme/, xfconf/     # look and layout of the desktop
│   └── seccomp.json        # Docker's default profile + namespaces for the browser sandbox
├── mcp/                    # the MCP server (Python, official MCP SDK)
│   ├── bot_station_mcp/    # tools, HTTP client, certificate pinning
│   ├── tests/smoke_test.py # calls every tool against a running computer
│   └── Dockerfile
├── viewer/index.html       # reference control page + agent API test panel
├── tools/                  # benchmarks (CPU, RAM, browser fingerprint)
└── docs/
    ├── architecture.md     # design, protocol, security, measurements, gotchas
    └── api.md              # HTTP API reference (control + agent)
```

## Documentation

- [docs/architecture.md](docs/architecture.md): how it's built and why, the streaming protocol, security model, performance numbers and lessons learned
- [docs/api.md](docs/api.md): the HTTP API (`/api/...` control, `/agent/...` agent), for use without MCP
- [mcp/README.md](mcp/README.md): MCP tools, settings, running without Docker (stdio)
- [AGENTS.md](AGENTS.md): instructions for AI agents setting up or working on this repository
- [CONTRIBUTING.md](CONTRIBUTING.md): development setup and tests

## License

[MIT](LICENSE). Bot Station bundles the [Helium](https://github.com/imputnet/helium-linux)
browser, downloaded at build time under its own license.
