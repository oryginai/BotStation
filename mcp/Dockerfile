# Bot Station MCP server, serving Streamable HTTP on :8765 (/mcp).
# Built and run by the repository's compose.yaml, next to the computer.
FROM python:3.13-slim
COPY --from=ghcr.io/astral-sh/uv:0.8.5 /uv /bin/uv
WORKDIR /app
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never

# Dependencies first (cached while only the code changes), then the package
COPY pyproject.toml uv.lock README.md ./
RUN uv sync --frozen --no-dev --no-install-project
COPY bot_station_mcp/ ./bot_station_mcp/
RUN uv sync --frozen --no-dev && useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin mcp

USER mcp
ENV PATH=/app/.venv/bin:$PATH MCP_TRANSPORT=streamable-http MCP_HOST=0.0.0.0 MCP_PORT=8765
EXPOSE 8765
CMD ["bot-station-mcp"]
