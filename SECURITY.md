# Security policy

Bot Station hands an AI agent a desktop with a logged-in browser, so security
bugs here matter. Thank you for reporting them privately.

## Reporting a vulnerability

Please **don't open a public issue**. Use GitHub's
[private vulnerability reporting](../../security/advisories/new) for this
repository instead. Include what you found, how to reproduce it, and what an
attacker gains. We aim to answer within a week.

## In scope

- Getting past token checks on desktopd (`:8443`) or the MCP server (`:8765`)
- Escaping the browser sandbox or the container's restrictions
- Leaking a token (`CONTROL_TOKEN`, `AGENT_TOKEN`, `MCP_TOKEN`, viewer tokens)
- A web page reaching the agent API, the MCP endpoint or the browser's
  debugging port

## Not vulnerabilities

- Whoever holds `MCP_TOKEN` or `AGENT_TOKEN` can control the desktop. That
  is the design.
- `AGENT_EXEC=true` gives agents a root shell in the container, by design.
- Running with `BIND_ADDRESS=0.0.0.0` and no firewall or proxy in front.

The security model is described in
[docs/architecture.md](docs/architecture.md#security).
