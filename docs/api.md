# HTTP API reference

The HTTPS API served by `desktopd` on port 8443. It has three parts:
- **Control API** (`/api/...`): turns the live view on and off and creates viewer sessions.
- **Agent API** (`/agent/...`): lets AI agents see and drive the computer: screen, input, windows, clipboard, apps, shell, and the browser's pages by structure.
- **Viewer** (`/stream`, `/view`, `/client.js`): the live picture in a browser.

Base URL in the examples: `https://localhost:8443`.

## Basics

### Authentication

Every `/api/` and `/agent/` call needs a bearer token:

```
Authorization: Bearer <token>
```

| Token | Set with | Works on |
|---|---|---|
| Control token | `CONTROL_TOKEN` (required, ≥32 chars) | `/api/...` and `/agent/...` |
| Agent token | `AGENT_TOKEN` (optional, ≥32 chars) | `/agent/...` only |

Give agents and the MCP server the agent token, never the control token. Make a token with `openssl rand -hex 32`.

### Transport and limits

- **TLS 1.3 only.** Without `TLS_CERT`/`TLS_KEY` the certificate is self-signed: accept it once by opening `/healthz`, or use `curl -k`.
- **Brute force:** after 10 failed auths in a minute from one IP, every call from that IP gets `429` for the rest of that minute.
- **CORS:** `/api/`, `/agent/` and `/client.js` answer the origins in `ALLOWED_ORIGINS` (default `*`). Auth is a header and never a cookie, so `*` doesn't expose credentials. Screenshot size headers are exposed to browsers.
- **Request bodies:** JSON with `Content-Type: application/json`. A body may be left out where every field is optional.

### Errors

Errors have a non-2xx status and this body:

```json
{ "error": "human-readable message" }
```

| Status | Meaning |
|---|---|
| 400 | Bad input: invalid JSON, unknown action, bad value, stale element ref |
| 401 | Missing or wrong token |
| 403 | Feature turned off (`/agent/exec` without `AGENT_EXEC=true`, `/agent/browser/...` with `BROWSER_AUTOMATION=false`) |
| 404 | No such route. The whole `/agent/` tree returns 404 when `AGENT_API=false` |
| 409 | Streaming is disabled (control API / viewer only) |
| 429 | Too many failed auth attempts |
| 503 | Too many viewers; or the browser isn't running (or was started without automation) |
| 504 | The clipboard owner didn't answer |

### Coordinates

All positions are **screen pixels** on the 1280×720 display, origin top-left. Screenshots, actions and windows all use this space. When a screenshot is scaled, divide its pixel positions by `scale` to get screen coordinates.

---

## Health

### `GET /healthz`

No auth. Returns `ok` (plain text). Use it for liveness checks and to accept the self-signed certificate.

---

## Control API

The live view (streaming) starts **disabled** unless `STREAM_ENABLED=true`. The agent API works whether streaming is on or off.

### `GET /api/status`

```json
{ "enabled": false, "viewers": 0, "width": 1280, "height": 720 }
```

### `POST /api/enable`

Allows viewers. Returns the same body as status.

### `POST /api/disable`

Stops streaming: disconnects every viewer and voids unused viewer tokens. Returns the same body as status.

### `POST /api/sessions`

Creates a single-use viewer token.

Request (optional):

```json
{ "view_only": false }
```

Response `201`:

```json
{
  "token": "9f2c…",
  "expires_at": "2026-10-16T10:00:00Z",
  "view_only": false,
  "stream_path": "/stream",
  "view_url": "https://localhost:8443/view#token=9f2c…"
}
```

- Tokens work **once** and expire after `VIEWER_TOKEN_TTL` seconds (default 60).
- Returns `409` while streaming is disabled.
- `view_url` is built from `PUBLIC_URL` when that is set.

---

## Viewer

| Path | What |
|---|---|
| `GET /view#token=…` | The desktop alone in a page, scaled to fit, with a full-screen button. The token sits in the `#fragment`, so it never reaches a server. |
| `GET /client.js` | ES module for embedding the viewer in your own page. |
| `GET /stream` | WebSocket used by the client. The token travels in the subprotocol, never in the URL. |

Embedding in your own page:

```js
import { connectDesktop } from 'https://localhost:8443/client.js';

const s = await (await fetch('/your-backend/session')).json(); // your backend calls POST /api/sessions
const desktop = connectDesktop(canvas, 'wss://localhost:8443' + s.stream_path, s.token, {
  onOpen() {},
  onClose(reason, clean) {},
});
desktop.close();
```

The wire protocol is documented at the top of `server/stream.go`.

---

## Agent API

| Call | Purpose |
|---|---|
| `GET /agent/status` | Screen, pointer, whether the browser is open, shell state |
| `GET /agent/screenshot` | Image of the screen or a region |
| `POST /agent/actions` | Mouse, keyboard and wait steps, optional screenshot after |
| `GET /agent/windows` | List windows |
| `POST /agent/windows` | Activate, close, minimize, maximize, move, resize |
| `GET /agent/clipboard` | Read the clipboard |
| `POST /agent/clipboard` | Set the clipboard |
| `POST /agent/launch` | Start the browser, terminal or files |
| `POST /agent/exec` | Run a shell command (off by default) |
| `GET /agent/browser/tabs` | List the browser's tabs |
| `POST /agent/browser/tabs` | New, activate, close tab |
| `GET /agent/browser/snapshot` | Interactive elements of the page with boxes on screen (+ text) |
| `POST /agent/browser/navigate` | Go to a URL, back, forward, reload |
| `POST /agent/browser/click` | Click an element by ref |
| `POST /agent/browser/fill` | Type into an element by ref, or pick a `<select>` option |
| `POST /agent/browser/wait` | Wait for load, network idle or some text |
| `POST /agent/browser/dialog` | Answer an alert, confirm or prompt |

### `GET /agent/status`

```json
{
  "width": 1280, "height": 720,
  "pointer": { "x": 640, "y": 360 },
  "browser": { "running": true, "automation": true },
  "exec": false
}
```

`browser.running` is true while a Helium window is open. `browser.automation` says whether the `/agent/browser/` calls are on (`BROWSER_AUTOMATION`).

### `GET /agent/screenshot`

Returns the image bytes (`image/jpeg` or `image/png`).

| Query | Default | Notes |
|---|---|---|
| `format` | `jpeg` | `jpeg` or `png` |
| `quality` | `80` | JPEG quality, 1–100 |
| `scale` | `1` | 0.1–1 (box-filtered downscale) |
| `x`, `y`, `w`, `h` | whole screen | Region in screen pixels, clipped to the screen |
| `cursor` | `true` | Draw the mouse pointer |

Response headers:
- `X-Image-Width`, `X-Image-Height`: size of the returned image.
- `X-Screen-Width`, `X-Screen-Height`: size of the screen.
- `X-Scale`: the scale applied.

Typical cost: about 25 ms; about 60 KB at scale 0.75 as JPEG.

```bash
curl -sk -H "Authorization: Bearer $TOKEN" \
  "https://localhost:8443/agent/screenshot?scale=0.5" -o screen.jpg
```

### `POST /agent/actions`

Runs up to 100 steps in order, in one round trip. It can return a screenshot taken after the last step, so one call can act and then verify.

```json
{
  "actions": [
    { "type": "click", "x": 400, "y": 300 },
    { "type": "type", "text": "hello ✓" },
    { "type": "key", "keys": "enter" },
    { "type": "wait", "until": "stable" }
  ],
  "screenshot": { "scale": 0.5 }
}
```

`screenshot` is optional and takes the same fields as the screenshot query: `format`, `quality`, `scale`, `x`, `y`, `w`, `h`, `cursor`.

Response `200`:

```json
{
  "done": 4,
  "results": [null, null, null, { "until": "stable", "ok": true, "waited_ms": 720 }],
  "pointer": { "x": 400, "y": 300 },
  "screenshot": { "mime": "image/jpeg", "width": 640, "height": 360, "scale": 0.5, "data": "<base64>" }
}
```

- `results[i]` is `null`, except for `wait` steps.
- If a step fails, the call stops there with `400`: `{ "error": "action 2 (key): unknown key \"enterr\"", "done": 2, "results": [...] }`. Steps before it have already happened.

#### Step types

`x`/`y` are optional on clicks, buttons and scroll. Without them the step acts at the current pointer position. Coordinates outside the screen are clamped to its edge.

| `type` | Fields | Notes |
|---|---|---|
| `move` | `x`, `y` | Move the pointer |
| `click` | `x`?, `y`?, `button`?, `clicks`? | `button`: `left` (default), `middle`, `right`. `clicks`: 1–3 |
| `double_click` | `x`?, `y`?, `button`? | |
| `down` / `up` | `x`?, `y`?, `button`? | Press or release a button (for custom drags) |
| `drag` | `x`, `y`, `to_x`, `to_y`, `button`? | Press, move in steps, release |
| `scroll` | `x`?, `y`?, `dy`?, `dx`? | Wheel clicks: `dy` > 0 scrolls down, `dx` > 0 scrolls right. Default `dy: 3`, max 50 |
| `type` | `text`, `delay_ms`? | Any Unicode text, up to 192 KB; `\n` presses Enter, `\t` presses Tab. `delay_ms`: 0–200 between characters |
| `key` | `keys` | Press and release a combo, e.g. `ctrl+l`, `ctrl+shift+t`, `alt+tab`, `f5` |
| `key_down` / `key_up` | `keys` | Hold or release keys (e.g. shift while clicking) |
| `wait` | `until`?, `stable_ms`?, `timeout_ms`? | See below |
| `sleep` | `ms` | 0–30000 |
| `clipboard` | `text` | Set the clipboard |

**Key names** in `keys`:
- Modifiers: `ctrl`, `shift`, `alt`, `super` (aliases `control`, `option`, `meta`, `win`, `cmd`).
- Other keys: `enter`, `tab`, `esc`, `backspace`, `delete`, `insert`, `home`, `end`, `pageup`, `pagedown`, `up`, `down`, `left`, `right`, `space`, `capslock`, `menu`, `print`, `f1`–`f12`, `plus`, `minus`.
- Any single character, e.g. `ctrl+/`.
- Uppercase letters mean the same key as lowercase (`ctrl+T` = `ctrl+t`). Add `shift` explicitly when you need it.

**`wait`** watches the screen itself. No screenshots are taken or sent.

- `until: "stable"` (default): returns once nothing has changed for `stable_ms` (default 400, range 50–10000).
- `until: "change"`: returns as soon as anything changes.
- `timeout_ms`: default 5000, range 100–60000.

The result is `{ "until", "ok", "waited_ms" }`. `ok: false` means the timeout was reached. That is not an error.

### `GET /agent/windows`

```json
{
  "windows": [
    {
      "id": 12582933, "title": "Example Domain - Helium", "app": "Helium", "pid": 131,
      "x": 0, "y": 30, "width": 1280, "height": 638,
      "active": true, "minimized": false, "maximized": true
    }
  ]
}
```

- `app` is the window class: `Helium`, `Xfce4-terminal` or `Thunar`.
- The top bar and dock are not listed.
- `x`/`y`/`width`/`height` are the window's content area, without the title bar.

### `POST /agent/windows`

```json
{ "id": 12582933, "action": "maximize" }
```

| `action` | Extra fields |
|---|---|
| `activate` | Bring to front and focus |
| `close` | Politely close (the app may ask to confirm) |
| `minimize` | |
| `maximize`, `unmaximize` | |
| `move` | `x`, `y`: the frame's top-left, including the title bar |
| `resize` | `width`, `height` (apps may round, e.g. terminals to whole characters) |

Returns `{ "ok": true }`, or `400` for an unknown id or action.

### `GET /agent/clipboard`

Returns `{ "text": "…" }`: whatever was last copied in the desktop. It is `""` when the clipboard is empty or holds something other than text.

### `POST /agent/clipboard`

Sets the clipboard. Body: `{ "text": "…" }`, up to 192 KB. Paste it with a `key` step: `ctrl+v` in most apps, `ctrl+shift+v` in the terminal.

### `POST /agent/launch`

| `app` | Optional field | Notes |
|---|---|---|
| `browser` (alias `helium`) | `url` (`http://`, `https://` or `about:blank`) | Opens a new tab if the browser is already running |
| `terminal` | `path`: working directory | |
| `files` | `path`: folder to open | |

Returns `{ "ok": true, "pid": 131 }`. The app takes a moment to appear. Follow with a `wait` step (`until: "change"`, then `until: "stable"`), or poll `/agent/windows`.

### `POST /agent/exec`

Runs a shell command as root with `bash -lc`. **Off unless `AGENT_EXEC=true`** (otherwise `403`).

```json
{ "command": "git --version", "timeout": 30, "cwd": "/root" }
```

- `timeout`: seconds, default 30, max 600. On timeout the whole process group is killed.
- `cwd`: default `/root`.

```json
{
  "exit_code": 0, "stdout": "git version 2.51.0\n", "stderr": "",
  "truncated": false, "timed_out": false, "duration_ms": 12
}
```

`stdout` and `stderr` are capped at 256 KB each; `truncated` is set when either was cut. A non-zero exit code is still a `200`.

### Browser

The browser calls drive Helium over the Chrome DevTools Protocol (CDP), so agents can work from the page's structure instead of pixels.
- **Debugging port:** every Helium start goes through `helium.sh`, which opens the port on `127.0.0.1:9222`. It is reachable only inside the container and is never published.
- **Real input:** clicks and typing still go through the real mouse and keyboard at the element's position on screen. Pages get ordinary input events. Before acting, the call brings the tab and its window to the front.
- **Browser not running:** browser calls return `503`, except `navigate` with a `url` and `tabs` `new`, which start it first. If Helium was started while `BROWSER_AUTOMATION` was off, close it and start it again.
- **Tabs:** the `tab` field is optional everywhere. It defaults to the **active tab**, the one shown in the front browser window.
- **Open dialogs:** while an alert, confirm or prompt is open, scripts in that tab are paused. `snapshot` then returns just the `dialog`, and other calls fail with `400` until it is answered with `/agent/browser/dialog`.

### `GET /agent/browser/tabs`

```json
{ "tabs": [ { "id": "8B9A8998…", "url": "https://duckduckgo.com/", "title": "DuckDuckGo", "active": true } ] }
```

### `POST /agent/browser/tabs`

| Body | Effect |
|---|---|
| `{ "action": "new", "url"?: "…" }` | Opens a tab, loads `url` and waits for it. Returns `{ "ok": true, "id": "…", "tab": {…} }` |
| `{ "action": "activate", "id": "…" }` | Shows the tab and brings its window to the front |
| `{ "action": "close", "id": "…" }` | Closes the tab |

### `GET /agent/browser/snapshot`

Lists the page's interactive elements: links, buttons, form fields, ARIA widgets, and anything with a click handler or a tab index. Typical cost: 10–30 ms, about 100 ms for a long Wikipedia article with `all=true`.

| Query | Default | Notes |
|---|---|---|
| `tab` | active tab | |
| `all` | `false` | `true` also includes elements scrolled out of view (marked `offscreen`) |
| `text` | `0` | Also return the page's visible text, up to this many characters (max 200000) |

```json
{
  "tab": "8B9A8998…",
  "url": "https://duckduckgo.com/",
  "title": "DuckDuckGo",
  "viewport": { "x": 4, "y": 96, "width": 1272, "height": 568 },
  "scroll": { "y": 0, "height": 9118 },
  "elements": [
    { "ref": 1, "role": "link", "name": "Learn about DuckDuckGo", "href": "https://duckduckgo.com/about",
      "x": 68, "y": 128, "width": 209, "height": 54 },
    { "ref": 2, "role": "radio", "name": "Search", "checked": true, "x": 759, "y": 141, "width": 94, "height": 28 },
    { "ref": 4, "role": "combobox", "name": "Search with DuckDuckGo", "expanded": false, "focused": true,
      "x": 343, "y": 139, "width": 406, "height": 32 }
  ],
  "text": "…"
}
```

**Element fields:**

| Field | Meaning |
|---|---|
| `ref` | Id for `click` and `fill`. Stays the same for the same element while the page is loaded, and is invalid after navigation |
| `role` | `link`, `button`, `textbox`, `searchbox`, `combobox`, `listbox`, `checkbox`, `radio`, `slider`, `spinbutton`, `label`, `file`, `clickable`, or an explicit ARIA role (`tab`, `menuitem`, `option`, `switch`…) |
| `name` | Accessible name (aria-label, label, text, title, placeholder, alt…), max 120 characters |
| `value` | Current value of fields (a `<select>`'s chosen option text). Passwords show as `••••` |
| `href` | Link target |
| `checked` | Checkboxes, radios and ARIA-checked elements |
| `expanded` | Menus, comboboxes and `<details>` that can open |
| `disabled`, `focused`, `offscreen` | Present only when true |
| `x`, `y`, `width`, `height` | Box on **screen** (usable directly in `/agent/actions`) |

**Page fields:**
- `viewport`: where the page itself is drawn on screen.
- `scroll`: scroll position and total page height, in page pixels.
- `dialog`: only while a dialog is open: `{ "type": "alert|confirm|prompt|beforeunload", "message", "default_prompt"? }`.
- `challenge`: only while a human check is on the page and not yet solved: `cloudflare` (a "Just a moment" page or a Turnstile widget), `recaptcha` or `hcaptcha`. Agents shouldn't try to solve these. A Cloudflare page often clears by itself within a few seconds; if it doesn't, a person solves it in the live view. The browser profile keeps the result, so a site usually asks only once.

**Accuracy:** boxes match the screen pixel for pixel at any window position and size and at any page zoom.

**What gets filtered out:**
- Invisible elements.
- Elements covered by something else, such as a dialog or cookie banner. If something seems missing, deal with the overlay first.

**Not covered:** content inside `<iframe>`s and canvas-drawn UIs. Use screenshots and coordinates for those.

### `POST /agent/browser/navigate`

Exactly one of these is needed: `url`, `back`, `forward` or `reload`. `tab` is optional.

```json
{ "url": "github.com" }
```

- **URL handling:** a URL without a scheme gets `https://`. Only `http(s)://` and `about:blank` are allowed. Navigation waits until the page has loaded (30 s limit). A page that can't load (DNS, TLS…) is a `400` with Chromium's reason, e.g. `net::ERR_NAME_NOT_RESOLVED`.
- **Browser start:** if the browser isn't running, it is started first.
- **Back and forward:** `{ "back": true }` and `{ "forward": true }` return once the other page is shown, not when it has loaded. Follow them with `/agent/browser/wait` if needed.
- **Reload:** `{ "reload": true }` reloads the page and waits for it to load.

Response: `{ "ok": true, "tab": { "id", "url", "title", "active" } }`.

### `POST /agent/browser/click`

```json
{ "ref": 4, "button": "left", "clicks": 1 }
```

1. Shows the tab and brings the browser window to the front.
2. Scrolls the element into view if needed.
3. Clicks its centre with the **real mouse**.

Returns `{ "ok": true, "x": 545, "y": 155 }` (the point clicked).

Errors (`400`):
- Stale ref: "the page changed and that element is gone: take a new snapshot".
- Covered element: "the element is covered by something else (a dialog or banner?)".

### `POST /agent/browser/fill`

```json
{ "ref": 4, "text": "agent computer", "clear": true, "submit": true }
```

- Clicks the element, selects all and deletes it (unless `"clear": false`), then types `text` with the real keyboard (any Unicode, up to 192 KB).
- `"submit": true` presses Enter afterwards.
- For a native `<select>`, `text` is the option's value or visible text, and the option is picked directly (no popup). A wrong option gives `400` with the list of valid options.
- Checkboxes, radios, buttons and file inputs can't be filled: use `click`.

Returns `{ "ok": true }`. The call doesn't wait for the page to react. Follow with `/agent/browser/wait`, then a new snapshot.

### `POST /agent/browser/wait`

```json
{ "until": "idle", "timeout_ms": 10000 }
```

| `until` | Returns when |
|---|---|
| `load` (default) | The document has finished loading |
| `idle` | Loaded, and no network request for 500 ms. Requests open longer than 10 s (long polling, streams) don't count |
| `text` | The page's visible text contains `text` |

`timeout_ms`: default 10000, range 100–60000. The result is `{ "until", "ok", "waited_ms" }`. `ok: false` means the timeout was reached. That is not an error.

### `POST /agent/browser/dialog`

```json
{ "accept": true, "text": "answer for a prompt" }
```

Answers the open alert, confirm, prompt or "leave page?" dialog in the tab. `accept: false` presses Cancel. `text` fills a prompt. Returns `{ "ok": true }`, or `400` if no dialog is open.

## Recipes

### See, act, verify, in one round trip

```bash
curl -sk -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  https://localhost:8443/agent/actions -d '{
    "actions": [
      {"type": "click", "x": 640, "y": 52},
      {"type": "key", "keys": "ctrl+l"},
      {"type": "type", "text": "example.com\n"},
      {"type": "wait", "until": "stable", "timeout_ms": 8000}
    ],
    "screenshot": {"scale": 0.5}
  }'
```

### Browser task by structure, screenshot only to check

```bash
A=https://localhost:8443; H=(-sk -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json')
curl "${H[@]}" $A/agent/browser/navigate -d '{"url": "duckduckgo.com"}'
curl "${H[@]}" $A/agent/browser/snapshot                   # find the search box: ref 4
curl "${H[@]}" $A/agent/browser/fill -d '{"ref": 4, "text": "agent computer", "submit": true}'
curl "${H[@]}" $A/agent/browser/wait -d '{"until": "idle"}'
curl "${H[@]}" "$A/agent/browser/snapshot?text=2000"      # results, with the page text
```

### Recommended agent loop

1. **See:** take a screenshot (scale 0.5–0.75 keeps it cheap).
2. **Act:** send the steps in one `/agent/actions` call: clicks at coordinates, typing, key combos.
3. **Settle:** end the call with a `wait` step (`until: "stable"`) rather than fixed sleeps, and ask for the screenshot in the same call to verify.

In the browser, prefer the structure: `snapshot` → `click`/`fill` by ref → `wait` → `snapshot`. It costs no image tokens and doesn't miss by a few pixels. Fall back to screenshots and coordinates for canvas UIs, iframes and anything outside the page.

---

## Configuration

Set in `.env` at the repository root; see [`.env.example`](../.env.example) for every option.

| Variable | Default | Effect |
|---|---|---|
| `CONTROL_TOKEN` | — (required) | Control API token, also valid on the agent API |
| `AGENT_TOKEN` | unset | Agent-only token |
| `AGENT_API` | `true` | `false` removes every `/agent/` route |
| `AGENT_EXEC` | `false` | Enables `POST /agent/exec` (root shell) |
| `BROWSER_AUTOMATION` | `true` | Opens Helium's debugging port (inside the container only) and enables `/agent/browser/...` |
| `AGENT_HUMAN_INPUT` | `true` | Agent input at a person's pace: the pointer travels to clicks on a curved path (cubic Bézier, sometimes S-shaped, bending up to 12% of the distance; a third of moves over 150 px overshoot by up to 18 px and correct back), with a minimum-jerk speed profile and uneven step timing, in ~0.25–0.5 s; drags never overshoot; keys come ~30 ms apart (varied; text over 300 characters is typed at once), wheel clicks 20–50 ms apart. `false`: instant |
| `TZ` | UTC | Timezone of the browser and desktop, e.g. `Asia/Kolkata`. Set it to the IP's region |
| `BROWSER_LANG` | `en-US,en` | Browser languages, most preferred first. Set it to the IP's region |
| `BROWSER_WEBGL` | `true` | WebGL in software (SwiftShader); page drawing stays in the cheaper software compositor |
| `BROWSER_PROXY` | unset | `scheme://host:port` proxy for the browser's traffic, WebRTC included. No credentials |
| `BROWSER_ADBLOCK` | `false` | Helium's built-in uBlock Origin. Set at each start (the entrypoint switches it in the profile) |
| `STREAM_ENABLED` | `false` | Viewers allowed at start |
| `MAX_VIEWERS` | `2` | Simultaneous viewers |
| `VIEWER_TOKEN_TTL` | `60` | Seconds a viewer token stays valid |
| `ALLOWED_ORIGINS` | `*` | CORS origins, comma-separated |
| `PUBLIC_URL` | from request | Base of `view_url` |

## Known limitations

- **Shared input.** The agent and a human viewer use the same mouse and keyboard. Using both at once mixes the input.
- **Datacenter IPs.** Many sites block cloud-provider IPs whatever the browser does ("Access denied"). Use `BROWSER_PROXY` with a fixed ISP/residential IP in the region of `TZ`/`BROWSER_LANG`. For Cloudflare sites, the lasting fix is a verified bot identity (Cloudflare's signed agents / Web Bot Auth), set up for the platform rather than in this container.
- **Browser structure stops at iframes.** Elements inside `<iframe>`s (embedded checkouts, some login widgets, captchas) and canvas-drawn apps aren't in snapshots. Use screenshots and coordinates there.
