"""HTTP client for Bot Station's agent API (docs/api.md)."""

from __future__ import annotations

import hashlib
import hmac
import os
import ssl
from dataclasses import dataclass
from typing import Any

import httpx


class APIError(Exception):
    """A failed agent API call, with the server's message."""


@dataclass
class Config:
    url: str
    token: str
    verify: ssl.SSLContext | bool

    @classmethod
    def from_env(cls) -> Config:
        url = os.environ.get("BOT_STATION_URL", "https://localhost:8443").rstrip("/")
        token = os.environ.get("BOT_STATION_TOKEN", "")
        if not token:
            raise SystemExit("BOT_STATION_TOKEN is not set (use the computer's AGENT_TOKEN)")
        return cls(url=url, token=token, verify=tls_context())


class _PinnedSSLObject(ssl.SSLObject):
    """Accepts the server only if its certificate has the pinned SHA-256."""

    pin: bytes = b""

    def do_handshake(self) -> None:
        super().do_handshake()
        der = self.getpeercert(binary_form=True) or b""
        if not hmac.compare_digest(hashlib.sha256(der).digest(), self.pin):
            raise ssl.SSLCertVerificationError(
                "the computer's TLS certificate does not match the pinned one (BOT_STATION_CERT_SHA256 / BOT_STATION_CERT_PIN_FILE)"
            )


def tls_context() -> ssl.SSLContext | bool:
    """How the computer's certificate is checked, from the environment.

    - BOT_STATION_CERT_SHA256: pin the certificate (for the self-signed
      one desktopd makes; it logs the fingerprint at startup).
    - BOT_STATION_CERT_PIN_FILE: pin the certificate in this PEM file (the
      Docker Compose setup mounts desktopd's own cert.pem read-only).
    - BOT_STATION_CA_CERT: trust this CA / certificate file.
    - BOT_STATION_INSECURE=true: no check at all (local testing only).
    - Otherwise: the system's trusted CAs (a real certificate).
    """
    digest = _pinned_digest()
    if digest:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE  # replaced by the pin check
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        ctx.sslobject_class = type("PinnedSSLObject", (_PinnedSSLObject,), {"pin": digest})
        return ctx
    if ca := os.environ.get("BOT_STATION_CA_CERT"):
        ctx = ssl.create_default_context(cafile=ca)
        ctx.minimum_version = ssl.TLSVersion.TLSv1_3
        return ctx
    if os.environ.get("BOT_STATION_INSECURE") == "true":
        return False
    ctx = ssl.create_default_context()
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


def _pinned_digest() -> bytes | None:
    """The SHA-256 to pin, from BOT_STATION_CERT_SHA256 or the certificate
    in BOT_STATION_CERT_PIN_FILE; None when neither is set."""
    if pin := os.environ.get("BOT_STATION_CERT_SHA256", "").replace(":", "").strip():
        try:
            digest = bytes.fromhex(pin)
        except ValueError:
            digest = b""
        if len(digest) != 32:
            raise SystemExit("BOT_STATION_CERT_SHA256 must be 64 hex characters (colons allowed)")
        return digest
    if path := os.environ.get("BOT_STATION_CERT_PIN_FILE"):
        try:
            with open(path) as f:
                der = ssl.PEM_cert_to_DER_cert(f.read())
        except (OSError, ValueError) as e:
            raise SystemExit(f"can't read the certificate in BOT_STATION_CERT_PIN_FILE ({path}): {e}") from e
        return hashlib.sha256(der).digest()
    return None


class AgentComputer:
    """Thin async wrapper over /agent/... calls."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self._http: httpx.AsyncClient | None = None

    @property
    def http(self) -> httpx.AsyncClient:
        if self._http is None:
            self._http = httpx.AsyncClient(
                base_url=self.cfg.url,
                headers={"Authorization": f"Bearer {self.cfg.token}"},
                verify=self.cfg.verify,
                timeout=httpx.Timeout(60.0, connect=10.0),
            )
        return self._http

    async def close(self) -> None:
        if self._http is not None:
            await self._http.aclose()

    async def _send(self, method: str, path: str, *, timeout: float | None = None, **kw: Any) -> httpx.Response:
        if timeout is not None:
            kw["timeout"] = httpx.Timeout(timeout, connect=10.0)
        try:
            res = await self.http.request(method, path, **kw)
        except httpx.ConnectError as e:
            if tls := _ssl_error(e):
                raise APIError(f"TLS check of {self.cfg.url} failed: {tls}") from e
            raise APIError(f"can't reach the computer at {self.cfg.url}: {e}") from e
        except httpx.TimeoutException as e:
            raise APIError(f"{method} {path} timed out") from e
        except httpx.HTTPError as e:
            raise APIError(f"{method} {path} failed: {e}") from e
        if res.is_success:
            return res
        try:
            msg = res.json().get("error") or res.text
        except ValueError:
            msg = res.text.strip() or res.reason_phrase
        if res.status_code == 401:
            msg = "the computer rejected the token (check BOT_STATION_TOKEN)"
        raise APIError(f"{msg} (HTTP {res.status_code})")

    async def get(self, path: str, params: dict[str, Any] | None = None, *, timeout: float | None = None) -> Any:
        res = await self._send("GET", path, params=_clean(params, query=True), timeout=timeout)
        return res.json()

    async def post(self, path: str, body: dict[str, Any] | None = None, *, timeout: float | None = None) -> Any:
        res = await self._send("POST", path, json=_clean(body) or {}, timeout=timeout)
        return res.json()

    async def screenshot(self, params: dict[str, Any]) -> tuple[bytes, httpx.Headers]:
        res = await self._send("GET", "/agent/screenshot", params=_clean(params, query=True))
        return res.content, res.headers


def _ssl_error(e: BaseException) -> str | None:
    """The TLS failure behind a connection error, if that is what it was."""
    seen = set()
    while e is not None and id(e) not in seen:
        seen.add(id(e))
        if isinstance(e, ssl.SSLCertVerificationError):
            return getattr(e, "verify_message", None) or (e.args[0] if e.args else str(e))
        if isinstance(e, ssl.SSLError):
            return str(e)
        e = e.__cause__ or e.__context__
    return None


def _clean(d: dict[str, Any] | None, query: bool = False) -> dict[str, Any] | None:
    """Drops unset (None) fields, so the server's defaults apply. In a query
    string, booleans are sent as "true"/"false"."""
    if d is None:
        return None
    out = {}
    for k, v in d.items():
        if v is None:
            continue
        out[k] = ("true" if v else "false") if query and isinstance(v, bool) else v
    return out
