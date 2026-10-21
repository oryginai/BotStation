"""End-to-end check against a running Bot Station, through the MCP server
over stdio, the way an agent's client talks to it.

    BOT_STATION_TOKEN=... BOT_STATION_CERT_SHA256=... python tests/smoke_test.py

Needs internet access in the computer (it loads duckduckgo.com). It opens
pages and types in the browser, so don't run it while someone is using the
desktop.
"""

from __future__ import annotations

import asyncio
import os
import sys
import time

from mcp import Client
from mcp.client.stdio import StdioServerParameters

failures = 0


def show(name: str, res, secs: float) -> str:
    text = "\n".join(c.text for c in res.content if c.type == "text")
    images = [c for c in res.content if c.type == "image"]
    status = "ERROR" if res.is_error else "ok"
    first = text.splitlines()[0] if text else ""
    print(f"{status:5} {name:22} {secs * 1000:6.0f} ms  {len(text):5} chars  {len(images)} img  {first[:90]}")
    return text


async def main() -> None:
    global failures
    params = StdioServerParameters(
        command=sys.executable, args=["-m", "bot_station_mcp.server"], env=dict(os.environ)
    )
    async with Client(params) as client:
        tools = (await client.list_tools()).tools
        print(f"{len(tools)} tools: {', '.join(t.name for t in tools)}\n")

        async def run(name: str, args: dict | None = None, expect_error: bool = False) -> str:
            global failures
            t0 = time.perf_counter()
            res = await client.call_tool(name, args or {})
            text = show(name, res, time.perf_counter() - t0)
            if res.is_error != expect_error:
                failures += 1
                print("      ^ unexpected:", text[:300])
            return text

        await run("computer_status")
        await run("screenshot", {"scale": 0.5})
        await run("screenshot", {"x": 0, "y": 0, "width": 400, "height": 30})
        snap = await run("browser_navigate", {"url": "duckduckgo.com"})
        ref = next(int(l[1 : l.index("]")]) for l in snap.splitlines() if l.startswith("[") and "combobox" in l)
        snap = await run("browser_fill", {"ref": ref, "text": "model context protocol", "submit": True})
        await run("browser_wait", {"until": "idle"})
        await run("browser_snapshot", {"text_chars": 500})
        await run("browser_tabs")
        new = await run("browser_new_tab", {"url": "example.com", "observe": "none"})
        tab_id = new.split()[2].rstrip(":")
        await run("browser_close_tab", {"id": tab_id})
        await run("browser_navigate", {"action": "back", "observe": "none"})
        await run("browser_click", {"ref": 99999}, expect_error=True)  # stale ref: error text for the agent
        await run("list_windows")
        await run("press_keys", {"keys": "ctrl+l", "observe": "none"})
        await run("type_text", {"text": "example.com\n"})
        await run("wait_for_screen", {"until": "stable"})
        await run("click", {"x": 640, "y": 400, "observe": "none"})
        await run("scroll", {"x": 640, "y": 400, "dy": 3, "observe": "none"})
        await run("run_actions", {"actions": [{"type": "move", "x": 100, "y": 100}, {"type": "wait", "until": "stable"}], "observe": "none"})
        await run("set_clipboard", {"text": "hello from MCP ✓"})
        clip = await run("get_clipboard")
        if clip != "hello from MCP ✓":
            failures += 1
            print("      ^ clipboard round trip failed:", repr(clip))
        await run("run_shell", {"command": "echo hi"}, expect_error="--exec" not in sys.argv)
    print(f"\n{'all good' if not failures else f'{failures} failure(s)'}")
    sys.exit(1 if failures else 0)


asyncio.run(main())
