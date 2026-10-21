# Contributing

Thanks for helping. Small, focused pull requests are the easiest to review.
For a larger change, open an issue first so we can agree on the approach.

## Development setup

Requirements: Docker with Compose v2.20+, Go 1.25+, and
[uv](https://docs.astral.sh/uv/) for Python.

```bash
./setup.sh --up                          # run the whole thing
docker compose up -d --build computer    # rebuild after changing computer/
docker compose up -d --build mcp         # rebuild after changing mcp/
```

To work on the MCP server without rebuilding its image, run it on the host
over stdio against the running computer. See
[mcp/README.md](mcp/README.md#without-docker-stdio).

## Checks

Run these before opening a pull request (CI runs the same ones):

```bash
(cd computer/server && go vet ./... && go test ./...)
(cd mcp && uvx ruff check . && uv run python -c "import bot_station_mcp.server")
docker compose config -q
```

If you changed behaviour agents rely on, also run the end-to-end smoke test
against a running computer ([mcp/README.md](mcp/README.md#testing)).

## Guidelines

- Keep the style of the code around you: short comments that say why, plain
  names, few dependencies.
- Update the docs in the same pull request: [docs/api.md](docs/api.md) for
  API changes, `.env.example` for new settings, and
  [docs/architecture.md](docs/architecture.md) for design changes or newly
  measured numbers.
- Performance claims need measurements. `tools/` has the scripts used so far.
- Never commit `.env`, tokens or certificates.
