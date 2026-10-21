# Architecture

How Bot Station is built, why, and what was measured along the way. For
setup, see the [README](../README.md).

**Bot Station** is a containerized Linux computer for AI agents and the
people supervising them. It is not an OS or a desktop environment of its own:
it is an image assembled from Ubuntu 26.04 and XFCE components (window
manager, panel, dock) with the Helium browser, Files and Terminal. The part built from
scratch is the remote-display layer: **desktopd** (our own streaming server
and protocol, replacing VNC) and its browser client.

The computer is viewed and controlled from a browser, with no third-party
branding. A token-protected API turns viewing on and off. An **agent API**
lets AI agents see and drive it without a viewer: screenshots, mouse,
keyboard, windows, clipboard, apps, and web pages by their structure. An
**MCP server** (`mcp/`, Python) exposes that API as tools for AI agents.

## Layout

| Path | What |
|---|---|
| `compose.yaml` | Runs both containers: `computer` and `mcp`. Ports, volumes, resource limits (values from `.env`) |
| `compose.disk-limit.yaml` | Optional disk cap (only on XFS+pquota / btrfs / zfs hosts) |
| `.env` / `.env.example` | Tokens + all options (`.env` is never committed or baked into an image) |
| `setup.sh`, `scripts/` | Token generation, MCP connect snippets (`connect.sh`), live-view link (`view.sh`) |
| `mcp/` | The MCP server (Python, `mcp` SDK 2.x): `bot_station_mcp/server.py` (tools, snapshot text format, agent instructions, HTTP auth), `client.py` (HTTP client, certificate pinning), `tests/smoke_test.py`, `Dockerfile`. See [mcp/README.md](../mcp/README.md) |
| `viewer/index.html` | Reference control page plus an agent API test panel (outside the image; the real one lives in the user's platform) |
| `tools/` | Not in any image. `measure.sh` (what the browser shows websites: `fingerprint.html`; CPU/RAM idle, scrolling `heavy.html`, WebGL `webgl.html`; results in `tools/results/`), `input-timing.sh` (agent click/typing speed), `realsite.sh` (CPU/RAM on a real site) |
| `computer/` | Everything that goes into the computer's image. The paths below are inside it |
| `Dockerfile` | Stage 1 builds the Go binaries; stage 2 downloads and verifies Helium; stage 3 is Ubuntu 26.04 + the desktop |
| `entrypoint.sh` | Prepares the home folders and browser profile, starts Xvfb (sets its refresh rate), xfwm4, wallpaper, xfce4-panel, desktopd; closes the browser on stop |
| `server/` | Go module `desktopd`: `main.go` (config, API, auth, sessions), `stream.go` (capture, tiles, JPEG, protocol), `xinput.go` (input, cursor), `clipboard.go`, `tls.go`, `agent.go` (agent API), `xagent.go` (typing, key combos, windows), `browser.go` (browser API over CDP), `cmd/wallpaper/` |
| `server/web/` | Embedded into desktopd: `client.js` (stream client), `view.html` + `view.js` (`/view` page) |
| `theme/` | GTK theme (`gtk/adw-gtk3-dark`), `gtk.css`, icons, launchers, wallpaper, dock config, app menu |
| `xfconf/` | XFCE defaults: panel/dock layout, xfwm4, terminal, Thunar |
| `helium/` | Helium browser: `helium.sh` launcher (runs it as the `browser` user, opens the local debugging port; timezone, language, WebGL and proxy flags), policies (`managed.json`, `recommended.json`), signing key |
| `seccomp.json` | Docker's default seccomp profile + user namespaces and mounts inside them, for the browser sandbox |

## Stack and why

- **Ubuntu 26.04 base**: XFCE 4.20, picom/docklike packaged, `apt` works out of the box. Debian and Alpine were tried; Alpine wasn't smaller in practice (graphics libs dominate).
- **Xvfb, at 1280x720 fixed**: a headless X server. Run with `-fbdir`, so its framebuffer is a memory-mapped file that desktopd reads directly. That means no screenshot round-trips. Note: `XGetImage` on the root window does not match the real screen when a compositor runs.
- **xfwm4 + xfce4-panel + docklike plugin**: a top bar (app menu, centered clock, show-desktop) and a bottom dock with dot indicators. There is no xfce4-session, xfce4-settings or xfdesktop.
- **`wallpaper` (our Go tool)**: sets the root window background and exits. It replaced xfdesktop (~45 MB) and uses 0 MB at rest.
- **No compositor**: picom gave rounded corners but cost several times the X server CPU whenever the screen changes (see Performance). Windows have square corners and the bars are solid.
- **Helium (Chromium-based, ungoogled)**: the only browser, and the default (`x-www-browser`, `mimeapps.list`, `$BROWSER`).
  - Measured against Firefox in this container: one Wikipedia tab ~550 MB vs ~700 MB; three tabs ~690 MB vs ~1.15 GB (Helium blocks ads); per frame drawn, the same CPU.
  - Downloaded from its GitHub releases at build time and checked against its signing key (`helium/signing-key.asc`). Updates mean bumping `HELIUM_VERSION` in the Dockerfile and rebuilding.
  - Policies (`/etc/chromium/policies`): memory saver on at Maximum (recommended, so users can change it), no metrics or promotions, downloads to `/root/Downloads`, `ForcedLanguages` from `BROWSER_LANG`.
  - **Built-in uBlock Origin off by default** (`BROWSER_ADBLOCK=true` turns it on). See Performance for what that costs and saves.
  - **Sandboxed**: Chromium refuses to sandbox itself as root, so `helium.sh` starts it as the unprivileged `browser` user (profile in the `bot-station-browser` volume). The sandbox needs user namespaces, which Docker's default seccomp profile blocks; `seccomp.json` allows them. `helium://sandbox` reports "adequately sandboxed".
  - Earlier: Firefox (heavier here), Chromium (~500 MB), Epiphany (heavier, broke TLS).
- **desktopd (Go, static binary)**: HTTPS/WSS server, control API, screen streaming, input injection (XTEST), cursor (XFIXES), clipboard bridge. Dependencies: `github.com/coder/websocket`, `github.com/jezek/xgb`.
- **Why not KasmVNC/noVNC/TigerVNC**: Kasm shows its own branding and client, and costs ~100 MB RAM per session plus ~300 MB disk (Perl + Mesa). TigerVNC has a compositing bug (stale black regions). We wanted our own client and protocol.
- **Why JPEG tiles, not H.264/WebRTC**: the expected usage is mostly static (agents, forms, text). Tiles cost ~0 when idle and keep text sharp. H.264 needs a continuously running software encoder; WebRTC adds STUN/TURN and complexity. Revisit only if smooth video is needed.

## How streaming works

1. Per viewer, up to `STREAM_FPS` (25) times/s, desktopd compares the framebuffer in 64x64 tiles with what that viewer last received.
2. Changed tiles are merged into horizontal runs, JPEG-encoded in parallel, and sent as one binary WebSocket message.
3. Adaptive quality: if a frame has ≥24 changed tiles (scrolling, dragging), they go at `STREAM_MOTION_QUALITY` (45). Each is re-sent at `STREAM_QUALITY` (85) once it has been still for 300 ms. Small changes (typing) are always sharp.
4. Flow control: at most 2 un-acked frames per viewer, so a slow link gets fewer frames instead of a backlog.
5. The pointer is hidden on the X side. Its shape is sent as a PNG and drawn locally by the browser, so there is no lag.
6. Nothing is captured or encoded while no viewer is connected.

Protocol (full spec at the top of `server/stream.go`):
- Auth: the client opens `wss://host/stream` with subprotocols `["desktop.v1", "token.<viewer token>"]`.
- Server → client:
  - text `hello` (size, view_only), `cursor` (PNG + hotspot), `clipboard`
  - binary frame `0x01, u16 count, {u16 x,y,w,h, u32 len, JPEG}…` (big-endian)
- Client → server, JSON:
  - `ack`, `r` (full refresh)
  - `m` (move x,y), `b` (button 1-3 down/up), `w` (wheel)
  - `k` (key by `KeyboardEvent.code`, mapped to evdev keycodes)
  - `clip` (set the desktop clipboard)

## Usage

```bash
./setup.sh --up           # .env with fresh tokens, then docker compose up -d --build
scripts/view.sh           # prints a one-time view_url (calls the API below)
# self-signed cert: the browser asks you to accept it once
```

API (all calls take `Authorization: Bearer $CONTROL_TOKEN`):

| Call | Effect |
|---|---|
| `POST /api/enable` | Allow viewers (streaming starts disabled) |
| `POST /api/disable` | Stop streaming, drop all viewers and pending tokens |
| `POST /api/sessions` `{"view_only":false}` | → `{token, expires_at, stream_path, view_url}` |
| `GET /api/status` | `{enabled, viewers, width, height}` |
| `GET /healthz` | No auth |

Viewing:
- `view_url` is `https://host:8443/view#token=…`: the desktop alone in a tab, scaled to fit, with a full-screen button.
- Custom pages: `import { connectDesktop } from 'https://host:8443/client.js'`. See `viewer/index.html`.
- Viewer tokens are single use and expire in `VIEWER_TOKEN_TTL` (60 s). A reload needs a new one.

## Agent API

Full reference: [api.md](api.md).

Auth: `Authorization: Bearer $AGENT_TOKEN` (or `$CONTROL_TOKEN`). It works
whether streaming is on or off. Coordinates are always screen pixels
(1280x720), shared by screenshots, actions and browser elements.

| Call | Effect |
|---|---|
| `GET /agent/status` | Screen size, pointer, browser running + automation on/off, exec on/off |
| `GET /agent/screenshot?format=jpeg\|png&quality=80&scale=1&x&y&w&h&cursor=true` | Image; size in `X-Image-*` / `X-Screen-*` headers |
| `POST /agent/actions` `{"actions":[…], "screenshot":{"scale":0.5}}` | Up to 100 steps in one round trip, optional screenshot (base64) after |
| `GET /agent/windows` / `POST /agent/windows` `{id, action}` | List; activate, close, minimize, maximize, unmaximize, move, resize |
| `GET` / `POST /agent/clipboard` | Read / set the desktop clipboard |
| `POST /agent/launch` `{"app":"browser\|terminal\|files", url?, path?}` | Start an app |
| `POST /agent/exec` `{command, timeout?, cwd?}` | Shell as root; **off** unless `AGENT_EXEC=true` |
| `GET` / `POST /agent/browser/tabs` | List; new (loads `url`), activate, close |
| `GET /agent/browser/snapshot?all&text` | Interactive elements: `ref`, role, name, value, box on screen; optional page text |
| `POST /agent/browser/navigate` `{url\|back\|forward\|reload}` | Waits for load; starts the browser if needed |
| `POST /agent/browser/click` / `fill` `{ref, …}` | Real mouse/keyboard at the element; `fill` picks `<select>` options directly |
| `POST /agent/browser/wait` `{until: load\|idle\|text}` | Page-level waits |
| `POST /agent/browser/dialog` `{accept, text?}` | Answer alert/confirm/prompt |

Action steps: `move`, `click` (button, clicks), `double_click`, `down`, `up`,
`drag` (to_x/to_y), `scroll` (dx/dy wheel clicks), `type` (any Unicode),
`key` (`"ctrl+shift+t"`), `key_down`, `key_up`, `wait` (`until: stable|change`,
stable_ms, timeout_ms), `sleep`, `clipboard`.

How it works:
- Input goes through XTEST (real input events), so it works on any app.
- `wait` compares framebuffer snapshots, so "act → wait until stable →
  verify" costs no screenshots.
- The browser calls use the Chrome DevTools Protocol on `127.0.0.1:9222`
  (opened by `helium.sh`, container-only). Page scripts run in an isolated
  world, so pages can't see or change the refs. Element boxes are converted
  to screen pixels from the window's box (asked of the browser over CDP,
  not the page's `screenX`/`outerWidth`, which a spoofing browser fakes),
  the page zoom and Helium's 4 px inset around the page. Clicks and typing still use XTEST at those
  boxes, after bringing the tab's window to the front.

Measured (local, 1280x720):

| Call | Time |
|---|---|
| Screenshot | ~25 ms (60 KB at 0.75 JPEG) |
| Click / type | <100 ms |
| Window list | ~5 ms |
| Browser snapshot | ~10–30 ms (~100 ms: a long Wikipedia article, all 1,188 elements) |
| Browser fill / click | ~150 ms (~300 ms when the window must be raised first) |
| Navigate, browser not running | ~2 s to a loaded page |

Typing: characters missing from the keymap are mapped onto spare keycodes on
the fly. The image sets `LANG=C.UTF-8`; without it, bash drops non-ASCII
input.

Focus pauses: apps move focus asynchronously after a shortcut or a click, so
`/agent/actions` waits 60 ms after each `key` and 30 ms after each click
(measured in Helium: with no gap, keys typed right after Ctrl+L were lost).

Clipboard: copying in the desktop goes to the local clipboard. Ctrl+V / Ctrl+Shift+V / Shift+Insert pastes the local clipboard into the desktop.

## MCP server

Full reference: [mcp/README.md](../mcp/README.md). Python, official `mcp` SDK
2.x (`MCPServer`), `httpx`. Its container serves Streamable HTTP on :8765
(`/mcp`); run outside Docker it defaults to stdio.

- 26 tools: one per agent API call, plus `click`/`type_text`/`press_keys`
  etc. as single steps and `run_actions` for batches.
- Each action takes `observe` (`screenshot`, `snapshot`, `both`, `none`):
  it waits for the screen to settle, then returns the screenshot or page
  snapshot in the same tool result. Input tools default to a screenshot,
  browser tools to a snapshot.
- Snapshots go to the agent as compact text (`[ref] role "name" state @
  centre x,y`), not JSON. A long page of results is ~3–4k characters.
- TLS: the self-signed certificate is pinned by SHA-256. In Compose the
  MCP container mounts the `bot-station-tls` volume read-only and pins
  `cert.pem` from it (`BOT_STATION_CERT_PIN_FILE`); outside Docker, pass the
  fingerprint from desktopd's startup log (`BOT_STATION_CERT_SHA256`).
  Python won't trust it as a CA: it isn't marked as one.
- HTTP auth: `MCP_TOKEN` as a bearer token (constant-time compare), and
  Host/Origin checks against `MCP_ALLOWED_HOSTS` against DNS rebinding.
  The MCP container runs as an unprivileged user, read-only, all
  capabilities dropped. It holds only `AGENT_TOKEN`, never `CONTROL_TOKEN`.
- Gotchas: in `mcp` 2.x, `FastMCP` is `MCPServer`, and result fields are
  snake_case (`is_error`). Only `ToolError` messages reach the agent; other
  exceptions are hidden, so every API error is turned into one.

## Security

- **TLS 1.3 only** (hybrid post-quantum key exchange). TLS 1.2 and plain HTTP are refused.
- **Certificate**: self-signed unless `TLS_CERT`/`TLS_KEY` are set; kept in the `bot-station-tls` volume. Use a real cert or an HTTPS proxy in production; `INSECURE_HTTP=true` only behind one.
- **Control token**: ≥32 chars is enforced, compared in constant time. It belongs on the platform backend and must never reach a browser.
- **Viewer tokens**: random 256-bit, single use, 60 s TTL. Carried in the WebSocket subprotocol, never in a URL. On `/view` it sits in the `#fragment`, is never sent to a server, and is wiped from the address bar.
- **Brute force**: 10 failed auths/min per IP → 429 for a minute. Behind a proxy all clients share one IP; X-Forwarded-For is not trusted yet.
- **Headers**: strict CSP on `/view`, HSTS, nosniff, no-referrer, `frame-ancestors` from `FRAME_ANCESTORS` (default `'none'`).
- **Server hardening**: header timeout, body and message size limits, `MAX_VIEWERS`.
- **Agent API**:
  - Use a separate `AGENT_TOKEN` (≥32 chars) so agents never hold the control token. It shares the brute-force limiter.
  - `AGENT_API=false` turns the whole API off. `/agent/exec` (root shell) is off unless `AGENT_EXEC=true`.
  - `launch` only starts the three apps and takes http(s) URLs only.
  - `BROWSER_AUTOMATION=false` closes Helium's debugging port and turns off `/agent/browser/`. The port listens on 127.0.0.1 inside the container only; any process in the container can use it, which is no more than root there can already do.
- **View-only sessions** get no input and no clipboard. `CLIPBOARD=both|to-desktop|from-desktop|off`.
- **Container**: `no-new-privileges`. Runs as root inside by design (user requirement); the browser runs as the unprivileged `browser` user with Chromium's full sandbox. `seccomp.json` = Docker's default + `clone`/`unshare`/`mount`/`umount2`/`pivot_root` so sandboxes can create namespaces (container root still can't mount: no `CAP_SYS_ADMIN`). GTK's image loaders now sandbox themselves too (bwrap).
- **`ALLOWED_ORIGINS` defaults to `*`** (OK because auth is a bearer token, not cookies). Set it to the platform origin.
- **SSH tunnelling**: possible as an admin path, but not for platform viewers (browsers can't speak SSH). TLS 1.3 already gives equivalent encryption.

## Size and startup (measured)

| | |
|---|---|
| Image, compressed (what a server pulls) | ~400 MB |
| Image, unpacked on disk | ~1.04 GB (Helium 387 MB, libraries 377 MB, `/usr/share` 88 MB) |
| `docker compose up` → API answering | ~0.7 s |
| → desktop drawn (bar and dock) | ~0.9 s |
| Browser launch → window open | ~1.0 s |
| Browser launch → page fully drawn | ~1.5–1.8 s |

(`docker images` on Docker Desktop shows ~1.5 GB: it counts the compressed
layers on top of the unpacked files.)

## Performance (measured, Xvfb CPU = % of one core)

| Scenario | No compositor (current) | picom (rounded corners) |
|---|---|---|
| Idle | 0% | 0% |
| Typing, ~5 chars/s | ~1% | ~3% |
| Typing, ~30 chars/s | ~4% | ~14% |
| Scrolling a full web page nonstop | ~5% | ~48% (95% with shadows) |

- picom's GLX backend uses ~388% CPU without a GPU.
- RAM: whole container idle ~90–110 MB; Helium with one Wikipedia tab ~480–550 MB (all its processes).
- **Display refresh (`DISPLAY_HZ`, default 25)**: Xvfb reports no refresh rate, so Chromium draws at 60 fps while anything moves. The entrypoint gives the display a 25 Hz mode (matching the stream's 25 fps), so Helium draws at 25. Helium CPU while scrolling: ~45% of a core at 60 Hz, ~30% at 30 Hz, ~25% at 25 Hz. Xvfb runs with `-noreset` (otherwise the server resets between startup clients and loses the mode).
- **Looking like a normal browser** (`TZ`, `BROWSER_LANG`, `BROWSER_WEBGL`, fonts, `AGENT_HUMAN_INPUT`), measured with `tools/measure.sh` before and after, 2 CPUs, a 720p heavy page:

  | | Before | After |
  |---|---|---|
  | Image (`docker images`) | 1.52 GB | 1.62 GB (+100 MB: fonts and tzdata; 1.79 GB with the CJK fonts added later) |
  | Idle CPU | 0.5% | 0.5% |
  | Scrolling CPU | ~29–30% | ~29–31% |
  | Container RAM, processes (anon) | ~380 MB | ~420 MB (+40 MB: the GPU process for WebGL) |
  | Browser cold start → page | ~0.4–0.55 s | ~0.43–0.63 s |
  | A page animating WebGL nonstop | no WebGL | ~24% CPU |
  | Agent click / typing 50 characters | 40 ms / 12 ms | ~490 ms / ~1.6 s (`AGENT_HUMAN_INPUT=false`: unchanged) |

  WebGL goes through SwiftShader, while the page is still drawn by the software compositor. With SwiftShader compositing too (plain hardware acceleration on), scrolling took ~200% CPU. Mesa (`--use-angle=gl`) gives no WebGL on Xvfb.
- **Ad blocker (`BROWSER_ADBLOCK`)**, A/B with a fresh container per run, two rounds, `tools/realsite.sh`:

  | | uBlock on | uBlock off (default) |
  |---|---|---|
  | Test page, no ads (`tools/measure.sh`), RAM anon | ~420 MB | ~315 MB |
  | theguardian.com, RAM anon / scroll CPU | ~437 MB / ~42% | ~322 MB / ~6% (but this is a consent wall: uBlock hides it, so the two columns show different pages) |
  | cnn.com, RAM anon / load CPU / scroll CPU | ~824 MB / ~118% / ~85% | ~1,105 MB / ~137% / ~120% (peak 1.57 GB of 2 GB) |

  uBlock's own extension process is ~85–120 MB. Pages with no ads come out ~100 MB lighter without it; ad-heavy pages cost ~280 MB more RAM and ~20–35% of a core more. For agents that roam ad-heavy sites under `MEMORY_LIMIT=2g`, turning it on is the safer choice.
- desktopd ~10–20 MB, plus ~3.6 MB per viewer. Polling costs ~2–5% CPU per connected viewer.
- Bandwidth: the first frame ~45–90 KB; idle sends nothing; scrolling a page is ~1 MB per 8 s burst.

Ideas not done yet:
- Use the XDamage extension instead of polling.
- libjpeg-turbo (via cgo).

## Persistence and limits

- **`bot-station-browser` volume = `/home/browser`**: the browser profile (logins, history, settings).
- **`bot-station-home` volume = `/root`**: files and app settings survive restart, `down`/`up` and image rebuilds. `docker compose down -v` deletes it.
- **Image-managed every start** (so updates apply): `gtk.css` (symlink to `/etc/desktop`), panel/dock and xfwm4 config. Bookmarks are copied only if missing.
- **Not persistent**: runtime `apt install` (add packages to the Dockerfile instead), `/tmp` (tmpfs), open windows.
- **Limits from `.env`** via compose: `CPU_LIMIT`, `MEMORY_LIMIT` (swap off), `PIDS_LIMIT`, `SHM_SIZE`, `TMP_SIZE`, `LOG_MAX_SIZE`, `BIND_ADDRESS`, `PORT`.
- **Disk**: Docker volumes can't be size-capped on ext4.
  - Options on a VPS: `compose.disk-limit.yaml` (needs overlay2 on XFS with `pquota`), XFS project quotas for volumes, or a fixed-size loop file / block device for `/root`.
  - Docker Desktop's containerd snapshotter supports none of these.

## Gotchas learned the hard way

- **xfwm4's built-in compositor**: froze KasmVNC's screen.
- **Compositors in general**: under TigerVNC they leave stale black regions. picom with no desktop window behind it leaves stale areas behind transparent windows.
- **Screenshots**: `xwd -root` / `GetImage` don't reflect compositor output; use the framebuffer file.
- **Process killing**: `pkill -f <pattern>` inside `sh -c '…<pattern>…'` kills the shell itself. Measure browser processes by command line, not `/proc/<pid>/exe`: root in the container can't read another user's `exe` or `smaps` (no `CAP_SYS_PTRACE`); run tools as `browser` (`docker exec -u browser`).
- **Builds**: on the dev network, IPv4 to `deb.debian.org` crawled; the mirror ARGs (`UBUNTU_MIRROR`) exist for that.
- **Docker env files**: no inline comments, no quotes.
- **Browser profile locks**: Chromium tags its lock with the hostname, and a recreated container used to get a new random one ("profile in use by another computer", browser won't start). Compose sets `hostname: bot-station`, and the entrypoint deletes stale `Singleton*` locks at start.
- **"Restore pages?" after every restart**: Chromium counts SIGTERM as a crash. The entrypoint marks the last session clean at start (the browser only ever stops with the container).
- **Xvfb resets when its last client disconnects**, dropping RandR modes set at startup: run it with `-noreset`.
- **Helium insets the page 4 px** on the sides and bottom (rounded page frame). Screen positions from `screenY + outerHeight - innerHeight` alone are 4 px too low.
- **Browser languages on Linux**: `--accept-lang` is ignored, so the list comes from the `ForcedLanguages` policy, which the entrypoint writes from `BROWSER_LANG`. Chromium then shows sites only the first language (`navigator.languages` = `["en-IN"]`); current Chrome does the same.
- **`HardwareAccelerationModeEnabled: false` means no WebGL at all** in current Chromium, which almost no real browser lacks. Use `--disable-gpu-compositing` with SwiftShader instead (see Performance).
- **Helium's uBlock Origin is a component extension**: `ExtensionInstallBlocklist`/`ExtensionSettings` policies, `--disable-extensions` and `--disable-component-extensions-with-background-pages` all leave it running. Its toggle in `helium://extensions` works: it stores `disable_reasons: [1]` (user action) under its id `blockjmkbacgjkknlgpkjjiijinjdanf` in the profile's `Preferences`. The entrypoint writes that before the browser starts (Linux doesn't enforce the prefs MACs); a new profile gets a `Preferences` with just that entry.
- **Requests cut off by a navigation are never reported finished** over CDP. Network-idle tracking drops the old document's requests when the main frame commits a new one, or `idle` waits run into the 10 s long-request cutoff.
