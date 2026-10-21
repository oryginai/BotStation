"""MCP server: Bot Station's agent API as tools for an AI agent.

Two ways to work, both on the same 1280x720 screen:
- Pixels: screenshots, mouse, keyboard, windows, clipboard, apps. Works on
  any app (terminal, files, the browser's own UI, canvas pages).
- Browser structure: tabs, page elements with refs, navigation, waits and
  dialogs. Cheaper and more exact for web pages.

Actions can return what the agent needs next (a screenshot or a page
snapshot) in the same tool call, so a step is one round trip.
"""

from __future__ import annotations

import argparse
import base64
import hmac
import logging
import os
from typing import Any, Literal

from mcp.server.mcpserver import Image, MCPServer
from mcp.server.mcpserver.exceptions import ToolError
from mcp.server.transport_security import TransportSecuritySettings
from mcp.types import ToolAnnotations

from .client import AgentComputer, APIError, Config

INSTRUCTIONS = """\
You control Bot Station, a Linux desktop (1280x720) with the Helium web browser, a terminal and a file manager.

Coordinates are always screen pixels, origin top-left. Full-size screenshots (scale 1, the default) use the same pixels, so read positions straight off the image.

For web pages, prefer the browser_* tools: browser_snapshot lists the page's interactive elements with a ref for each; act on them with browser_click / browser_fill by ref. They return a fresh snapshot, so you rarely need screenshots for web work. Use screenshots to check what the page looks like, or for content snapshots can't reach (iframes, canvas apps).

For everything else (terminal, files, dialogs outside the page, the browser's own toolbar), use screenshot + click / type_text / press_keys. These return a screenshot after acting by default.

Refs change after the page navigates: take a new snapshot rather than reusing old refs. A human may also be watching or using the desktop; don't fight over the mouse.

If a snapshot says a human check (Cloudflare, reCAPTCHA, hCaptcha) is on the page, don't try to solve or get around it. A Cloudflare "Just a moment" page often clears by itself: browser_wait a few seconds first. If it's still there, ask the user to solve it in the live view, then continue with browser_wait. The browser keeps the result, so a site usually asks only once.
"""

mcp = MCPServer("bot-station", instructions=INSTRUCTIONS)
_api: AgentComputer | None = None


def api() -> AgentComputer:
    global _api
    if _api is None:
        _api = AgentComputer(Config.from_env())
    return _api


async def call(fn, *args, **kwargs) -> Any:
    """Runs one API call; failures become tool errors the agent can read."""
    try:
        return await fn(*args, **kwargs)
    except APIError as e:
        raise ToolError(str(e)) from e


READ = ToolAnnotations(read_only_hint=True, open_world_hint=False)
ACT = ToolAnnotations(read_only_hint=False, destructive_hint=False, open_world_hint=True)
DANGER = ToolAnnotations(read_only_hint=False, destructive_hint=True, open_world_hint=True)


def tool(annotations: ToolAnnotations):
    return mcp.tool(annotations=annotations, structured_output=False)


# --- observing ---

Observe = Literal["screenshot", "snapshot", "both", "none"]

SETTLE = {"type": "wait", "until": "stable", "stable_ms": 300, "timeout_ms": 5000}


def shot_header(width: int, height: int, scale: float, region: tuple[int, int] | None = None) -> str:
    text = f"Screenshot {width}x{height}"
    if scale != 1:
        text += f" at scale {scale:g}: screen coordinates = image pixels / {scale:g}"
    else:
        text += ": image pixels are screen coordinates"
    if region:
        text += f" (region starting at screen {region[0]},{region[1]}: add that offset)"
    return text + "."


def shot_from_json(s: dict[str, Any]) -> list[Any]:
    fmt = "png" if s["mime"] == "image/png" else "jpeg"
    return [shot_header(s["width"], s["height"], s["scale"]), Image(data=base64.b64decode(s["data"]), format=fmt)]


async def take_screenshot(scale: float = 1, quality: int = 75, fmt: str = "jpeg", region=None, cursor=True) -> list[Any]:
    params: dict[str, Any] = {"format": fmt, "quality": quality, "scale": scale, "cursor": cursor}
    if region:
        params.update(x=region[0], y=region[1], w=region[2], h=region[3])
    data, h = await call(api().screenshot, params)
    w, hh = int(h.get("X-Image-Width", 0)), int(h.get("X-Image-Height", 0))
    return [shot_header(w, hh, float(h.get("X-Scale", scale)), region[:2] if region else None), Image(data=data, format=fmt)]


async def act(actions: list[dict[str, Any]], observe: Observe, *, settle: bool = True) -> list[Any]:
    """Runs input steps, then waits for the screen to settle and returns what
    observe asks for, all in as few calls as possible."""
    steps = list(actions)
    if settle and observe != "none":
        steps.append(SETTLE)
    body: dict[str, Any] = {"actions": steps}
    if observe in ("screenshot", "both"):
        body["screenshot"] = {"format": "jpeg", "quality": 75}
    res = await call(api().post, "/agent/actions", body, timeout=120)
    out: list[Any] = []
    waits = [r for r in res.get("results", []) if r]
    user_waits = waits[:-1] if (settle and observe != "none") else waits
    for w in user_waits:
        out.append(f"wait {w['until']}: {'ok' if w['ok'] else 'timed out'} after {w['waited_ms']} ms")
    n, p = len(actions), res.get("pointer")
    out.append(f"Done ({n} step{'s' if n != 1 else ''}). Pointer at {p['x']},{p['y']}." if p else "Done.")
    if "screenshot" in res:
        out += shot_from_json(res["screenshot"])
    if observe in ("snapshot", "both"):
        try:
            out.append(await snapshot_text())
        except ToolError as e:  # the action itself worked
            out.append(f"(No page snapshot: {e})")
    return out


async def settle_then(observe: Observe) -> list[Any]:
    """After a browser action: let the screen settle, then observe."""
    if observe == "none":
        return []
    return (await act([], observe))[1:]  # drop the "Done" line


# --- browser snapshot as compact text ---

def _short(s: str, n: int) -> str:
    return s if len(s) <= n else s[: n - 1] + "…"


def format_snapshot(s: dict[str, Any]) -> str:
    lines = [f'Tab {s["tab"]}: "{s.get("title", "")}" {s.get("url", "")}']
    if d := s.get("dialog"):
        lines.append(f'A {d["type"]} dialog is open: "{d["message"]}". Answer it with browser_dialog before anything else.')
        return "\n".join(lines)
    if c := s.get("challenge"):
        name = {"cloudflare": "Cloudflare", "recaptcha": "reCAPTCHA", "hcaptcha": "hCaptcha"}.get(c, c)
        lines.append(
            f"A {name} human check is on this page. Don't try to solve it. If it doesn't clear within a few seconds"
            " (browser_wait), ask the user to solve it in the live view, then browser_wait."
        )
    vp, sc = s["viewport"], s["scroll"]
    lines.append(
        f'Page on screen at {vp["x"]},{vp["y"]} size {vp["width"]}x{vp["height"]}; scrolled {sc["y"]} of {sc["height"]} px.'
    )
    els = s.get("elements") or []
    if not els:
        lines.append("No interactive elements visible (covered by an overlay, still loading, or inside an iframe/canvas?).")
    else:
        lines.append("Elements: [ref] role \"name\" details @ centre x,y on screen")
        for e in els:
            parts = [f'[{e["ref"]}] {e["role"]}']
            if e.get("name"):
                parts.append(f'"{e["name"]}"')
            if "value" in e:
                parts.append(f'value="{_short(e["value"], 80)}"')
            if "checked" in e:
                parts.append("checked" if e["checked"] else "unchecked")
            if "expanded" in e:
                parts.append("expanded" if e["expanded"] else "collapsed")
            for flag in ("disabled", "focused", "offscreen"):
                if e.get(flag):
                    parts.append(flag)
            if e.get("href"):
                parts.append(f'-> {_short(e["href"], 90)}')
            parts.append(f'@ {e["x"] + e["width"] // 2},{e["y"] + e["height"] // 2}')
            lines.append(" ".join(parts))
    if s.get("text"):
        lines.append("\nPage text:\n" + s["text"])
    return "\n".join(lines)


async def snapshot_text(tab: str | None = None, all: bool = False, text_chars: int = 0) -> str:
    s = await call(api().get, "/agent/browser/snapshot", {"tab": tab, "all": all, "text": text_chars or None})
    return format_snapshot(s)


# --- screen ---

@tool(READ)
async def computer_status() -> str:
    """Screen size, pointer position, whether the browser is open (and drivable by the browser_* tools), and whether run_shell is enabled."""
    s = await call(api().get, "/agent/status")
    b = s["browser"]
    return (
        f'Screen {s["width"]}x{s["height"]}, pointer at {s["pointer"]["x"]},{s["pointer"]["y"]}. '
        f'Browser {"open" if b["running"] else "closed"}, browser tools {"on" if b.get("automation") else "off"}. '
        f'run_shell {"enabled" if s["exec"] else "disabled"}.'
    )


@tool(READ)
async def screenshot(
    scale: float = 1,
    x: int | None = None,
    y: int | None = None,
    width: int | None = None,
    height: int | None = None,
    quality: int = 75,
    format: Literal["jpeg", "png"] = "jpeg",
) -> list[Any]:
    """Take a screenshot of the whole screen, or of a region (x, y, width, height in screen pixels) to zoom in on small text.

    scale (0.1-1) shrinks the image to save tokens; at scale 1 image pixels are screen coordinates. png is lossless but bigger.
    """
    region = (x, y, width, height) if None not in (x, y, width, height) else None
    return await take_screenshot(scale=scale, quality=quality, fmt=format, region=region)


# --- mouse & keyboard ---

@tool(ACT)
async def click(
    x: int,
    y: int,
    button: Literal["left", "right", "middle"] = "left",
    clicks: int = 1,
    observe: Observe = "screenshot",
) -> list[Any]:
    """Click at screen coordinates (clicks=2 for a double click, 3 to select a line). Returns a screenshot afterwards unless observe="none"."""
    return await act([{"type": "click", "x": x, "y": y, "button": button, "clicks": clicks}], observe)


@tool(ACT)
async def move_mouse(x: int, y: int, observe: Observe = "none") -> list[Any]:
    """Move the pointer without clicking (e.g. to open a hover menu or tooltip)."""
    return await act([{"type": "move", "x": x, "y": y}], observe)


@tool(ACT)
async def drag(x: int, y: int, to_x: int, to_y: int, button: Literal["left", "right", "middle"] = "left", observe: Observe = "screenshot") -> list[Any]:
    """Press at (x, y), move to (to_x, to_y) and release: drag and drop, selecting text, moving sliders."""
    return await act([{"type": "drag", "x": x, "y": y, "to_x": to_x, "to_y": to_y, "button": button}], observe)


@tool(ACT)
async def scroll(x: int, y: int, dy: int = 5, dx: int = 0, observe: Observe = "screenshot") -> list[Any]:
    """Scroll with the mouse wheel over (x, y). dy > 0 scrolls down, dy < 0 up; dx > 0 right. Units are wheel clicks (about 50-100 px each), max 50."""
    return await act([{"type": "scroll", "x": x, "y": y, "dy": dy, "dx": dx}], observe)


@tool(ACT)
async def type_text(text: str, observe: Observe = "screenshot") -> list[Any]:
    """Type text with the keyboard into whatever has focus (click the field first). Any Unicode. A newline presses Enter, a tab presses Tab."""
    return await act([{"type": "type", "text": text}], observe)


@tool(ACT)
async def press_keys(keys: str, observe: Observe = "screenshot") -> list[Any]:
    """Press a key or key combination, e.g. "enter", "esc", "ctrl+l", "ctrl+shift+t", "alt+tab", "f5", "ctrl+a".

    Names: ctrl, shift, alt, super, enter, tab, esc, backspace, delete, home, end, pageup, pagedown, up, down, left, right, space, f1-f12, or any single character.
    In the terminal, copy/paste are ctrl+shift+c / ctrl+shift+v.
    """
    return await act([{"type": "key", "keys": keys}], observe)


@tool(ACT)
async def run_actions(actions: list[dict[str, Any]], observe: Observe = "screenshot") -> list[Any]:
    """Run up to 100 input steps in one go, in order, then observe. Use it for sequences you are sure of.

    Steps (coordinates in screen pixels):
    {"type":"click","x","y","button"?,"clicks"?} {"type":"double_click","x","y"} {"type":"move","x","y"}
    {"type":"down"|"up","x"?,"y"?,"button"?} {"type":"drag","x","y","to_x","to_y"}
    {"type":"scroll","x","y","dy"?,"dx"?} {"type":"type","text"} {"type":"key","keys":"ctrl+l"}
    {"type":"key_down"|"key_up","keys":"shift"} {"type":"wait","until":"stable"|"change","timeout_ms"?}
    {"type":"sleep","ms"} {"type":"clipboard","text"}
    Example: [{"type":"key","keys":"ctrl+l"},{"type":"type","text":"example.com\\n"},{"type":"wait","until":"stable"}]
    """
    return await act(actions, observe)


@tool(READ)
async def wait_for_screen(until: Literal["stable", "change"] = "stable", timeout_ms: int = 5000, observe: Observe = "none") -> list[Any]:
    """Wait until the screen stops changing ("stable") or until anything changes ("change"), e.g. while an app starts or a page loads. Costs no screenshots unless observe asks for one."""
    return await act([{"type": "wait", "until": until, "timeout_ms": timeout_ms}], observe, settle=False)


# --- windows, apps, clipboard, shell ---

@tool(READ)
async def list_windows() -> str:
    """List open windows: id, app, title, position and size, and which is active."""
    ws = (await call(api().get, "/agent/windows"))["windows"]
    if not ws:
        return "No windows are open."
    return "\n".join(
        f'{w["id"]} {w["app"]} "{w["title"]}" at {w["x"]},{w["y"]} size {w["width"]}x{w["height"]}'
        + (" ACTIVE" if w["active"] else "")
        + (" minimized" if w["minimized"] else "")
        + (" maximized" if w["maximized"] else "")
        for w in ws
    )


@tool(ACT)
async def window_action(
    id: int,
    action: Literal["activate", "close", "minimize", "maximize", "unmaximize", "move", "resize"],
    x: int | None = None,
    y: int | None = None,
    width: int | None = None,
    height: int | None = None,
    observe: Observe = "screenshot",
) -> list[Any]:
    """Act on a window from list_windows. move takes x, y (top-left of the frame); resize takes width, height. close asks the app politely (it may ask to confirm)."""
    await call(api().post, "/agent/windows", {"id": id, "action": action, "x": x, "y": y, "width": width, "height": height})
    return ["OK.", *(await settle_then(observe))]


@tool(ACT)
async def launch_app(app: Literal["browser", "terminal", "files"], url: str | None = None, path: str | None = None, observe: Observe = "screenshot") -> list[Any]:
    """Start an app: the browser (optionally at an http(s) url; opens a new tab if it's already running), the terminal (optionally in path), or the file manager (optionally at path)."""
    r = await call(api().post, "/agent/launch", {"app": app, "url": url, "path": path})
    out = [f"Started {app} (pid {r.get('pid')})."]
    if observe != "none":
        # Wait for the window to appear, then for it to finish drawing
        res = await act([{"type": "wait", "until": "change", "timeout_ms": 5000}], observe)
        out += res[1:]
    return out


@tool(READ)
async def get_clipboard() -> str:
    """Read the desktop clipboard (whatever was last copied, as text)."""
    return (await call(api().get, "/agent/clipboard"))["text"]


@tool(ACT)
async def set_clipboard(text: str) -> str:
    """Put text on the desktop clipboard, e.g. to paste long text with ctrl+v (ctrl+shift+v in the terminal)."""
    await call(api().post, "/agent/clipboard", {"text": text})
    return "Clipboard set."


@tool(DANGER)
async def run_shell(command: str, timeout: int = 30, cwd: str | None = None) -> str:
    """Run a shell command as root on the computer (bash -lc) and return its output. Only works if the computer has AGENT_EXEC=true. timeout is in seconds (max 600)."""
    r = await call(api().post, "/agent/exec", {"command": command, "timeout": timeout, "cwd": cwd}, timeout=min(timeout, 600) + 15)
    out = [f"exit code {r['exit_code']}" + (" (timed out)" if r["timed_out"] else "") + f", {r['duration_ms']} ms"]
    if r["stdout"]:
        out.append("stdout:\n" + r["stdout"])
    if r["stderr"]:
        out.append("stderr:\n" + r["stderr"])
    if r["truncated"]:
        out.append("(output was cut at 256 KB)")
    return "\n".join(out)


# --- browser ---

BrowserObserve = Literal["snapshot", "screenshot", "both", "none"]


@tool(READ)
async def browser_tabs() -> str:
    """List the browser's tabs (id, title, url) and which one is active."""
    tabs = (await call(api().get, "/agent/browser/tabs"))["tabs"]
    if not tabs:
        return "No tabs open."
    return "\n".join(f'{t["id"]} "{t["title"]}" {t["url"]}' + (" ACTIVE" if t["active"] else "") for t in tabs)


@tool(READ)
async def browser_snapshot(text_chars: int = 0, include_offscreen: bool = False, tab: str | None = None) -> str:
    """List the interactive elements of the page (links, buttons, fields, checkboxes, menus…) with a ref for browser_click / browser_fill, their state and their centre on screen.

    text_chars > 0 also returns the page's visible text (up to that many characters), for reading content.
    include_offscreen adds elements scrolled out of view. Elements covered by a popup or banner are left out: deal with the overlay first.
    tab defaults to the active tab.
    """
    return await snapshot_text(tab, include_offscreen, text_chars)


@tool(ACT)
async def browser_navigate(
    url: str | None = None,
    action: Literal["go", "back", "forward", "reload"] = "go",
    tab: str | None = None,
    observe: BrowserObserve = "snapshot",
) -> list[Any]:
    """Load a url in the tab (https:// is added if missing; waits for the page to load; starts the browser if needed), or go back / forward / reload. Returns a snapshot of the new page by default."""
    if action == "go":
        if not url:
            raise ToolError('url is required for action "go"')
        body: dict[str, Any] = {"url": url}
    else:
        body = {action: True}
    body["tab"] = tab
    r = await call(api().post, "/agent/browser/navigate", body, timeout=60)
    t = r["tab"]
    return [f'Now at "{t["title"]}" {t["url"]}', *(await settle_then(observe))]


@tool(ACT)
async def browser_click(
    ref: int,
    button: Literal["left", "right", "middle"] = "left",
    clicks: int = 1,
    tab: str | None = None,
    observe: BrowserObserve = "snapshot",
) -> list[Any]:
    """Click a page element by its ref from the latest browser_snapshot. Scrolls it into view and clicks it with the real mouse. Returns a fresh snapshot by default (refs stay valid unless the page navigated)."""
    r = await call(api().post, "/agent/browser/click", {"ref": ref, "button": button, "clicks": clicks, "tab": tab})
    return [f"Clicked [{ref}] at {r['x']},{r['y']}.", *(await settle_then(observe))]


@tool(ACT)
async def browser_fill(
    ref: int,
    text: str,
    submit: bool = False,
    clear: bool = True,
    tab: str | None = None,
    observe: BrowserObserve = "snapshot",
) -> list[Any]:
    """Type text into a field by ref (replacing its content unless clear=false); submit=true presses Enter afterwards. For a dropdown (<select>), text is the option to pick (its visible text or value). Returns a fresh snapshot by default."""
    await call(api().post, "/agent/browser/fill", {"ref": ref, "text": text, "clear": clear, "submit": submit, "tab": tab})
    return [f"Filled [{ref}]" + (" and submitted." if submit else "."), *(await settle_then(observe))]


@tool(READ)
async def browser_wait(
    until: Literal["load", "idle", "text"] = "idle",
    text: str | None = None,
    timeout_ms: int = 10000,
    tab: str | None = None,
    observe: BrowserObserve = "none",
) -> list[Any]:
    """Wait for the page: "load" (document loaded), "idle" (loaded and no network activity for 500 ms), or "text" (the page shows the given text). A timeout is reported, not an error."""
    r = await call(api().post, "/agent/browser/wait", {"until": until, "text": text, "timeout_ms": timeout_ms, "tab": tab}, timeout=timeout_ms / 1000 + 15)
    out = [f"{until}: {'yes' if r['ok'] else 'timed out'} after {r['waited_ms']} ms."]
    if observe in ("snapshot", "both"):
        out.append(await snapshot_text(tab))
    if observe in ("screenshot", "both"):
        out += await take_screenshot()
    return out


@tool(ACT)
async def browser_dialog(accept: bool, text: str | None = None, tab: str | None = None, observe: BrowserObserve = "snapshot") -> list[Any]:
    """Answer the open alert / confirm / prompt / "leave page?" dialog: accept=true for OK, false for Cancel. text is the answer for a prompt."""
    await call(api().post, "/agent/browser/dialog", {"accept": accept, "text": text, "tab": tab})
    return ["Dialog answered.", *(await settle_then(observe))]


@tool(ACT)
async def browser_new_tab(url: str | None = None, observe: BrowserObserve = "snapshot") -> list[Any]:
    """Open a new tab (at url, waiting for it to load) and make it active. Starts the browser if needed."""
    r = await call(api().post, "/agent/browser/tabs", {"action": "new", "url": url}, timeout=60)
    t = r["tab"]
    return [f'New tab {t["id"]}: "{t["title"]}" {t["url"]}', *(await settle_then(observe))]


@tool(ACT)
async def browser_switch_tab(id: str, observe: BrowserObserve = "snapshot") -> list[Any]:
    """Show the tab with this id (from browser_tabs) and bring its window to the front."""
    await call(api().post, "/agent/browser/tabs", {"action": "activate", "id": id})
    return [f"Switched to tab {id}.", *(await settle_then(observe))]


@tool(ACT)
async def browser_close_tab(id: str) -> str:
    """Close the tab with this id (from browser_tabs)."""
    await call(api().post, "/agent/browser/tabs", {"action": "close", "id": id})
    return f"Closed tab {id}."


# --- entry point ---

log = logging.getLogger("bot_station_mcp")
LOCAL_HOSTS = "127.0.0.1:*,localhost:*,[::1]:*"


def http_app(host: str):
    """The Streamable HTTP app, behind a bearer token when MCP_TOKEN is set.

    Host/Origin headers are checked against MCP_ALLOWED_HOSTS (default: the
    local machine), which stops DNS rebinding from a web page even when the
    server listens on 0.0.0.0 inside a container."""
    hosts = [h.strip() for h in os.environ.get("MCP_ALLOWED_HOSTS", LOCAL_HOSTS).split(",") if h.strip()]
    security = TransportSecuritySettings(
        enable_dns_rebinding_protection="*" not in hosts,
        allowed_hosts=hosts,
        allowed_origins=[f"{scheme}://{h}" for h in hosts for scheme in ("http", "https")],
    )
    app = mcp.streamable_http_app(transport_security=security, host=host)
    token = os.environ.get("MCP_TOKEN", "")
    if not token:
        log.warning("MCP_TOKEN is not set: anyone who can reach this port controls the computer")
        return app
    if len(token) < 32:
        raise SystemExit("MCP_TOKEN must be at least 32 characters (openssl rand -hex 32)")
    return BearerAuth(app, token)


class BearerAuth:
    """ASGI middleware: HTTP requests need "Authorization: Bearer <token>"."""

    def __init__(self, app, token: str):
        self.app = app
        self.expected = f"Bearer {token}".encode()

    async def __call__(self, scope, receive, send):
        if scope["type"] == "http":
            got = dict(scope["headers"]).get(b"authorization", b"")
            if not hmac.compare_digest(got, self.expected):
                await send({"type": "http.response.start", "status": 401, "headers": [
                    (b"content-type", b"application/json"), (b"www-authenticate", b"Bearer")]})
                await send({"type": "http.response.body", "body": b'{"error":"missing or wrong MCP token"}'})
                return
        await self.app(scope, receive, send)


def main() -> None:
    p = argparse.ArgumentParser(description="MCP server for Bot Station")
    p.add_argument("--transport", choices=["stdio", "streamable-http"], default=os.environ.get("MCP_TRANSPORT", "stdio"))
    p.add_argument("--host", default=os.environ.get("MCP_HOST", "127.0.0.1"), help="streamable-http: address to listen on")
    p.add_argument("--port", type=int, default=int(os.environ.get("MCP_PORT", "8765")), help="streamable-http: port")
    args = p.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)  # not every request in the client's log
    api()  # fail early on missing configuration
    if args.transport == "stdio":
        mcp.run("stdio")
    else:
        import uvicorn

        log.info("MCP endpoint: http://%s:%d/mcp", args.host, args.port)
        uvicorn.run(http_app(args.host), host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
