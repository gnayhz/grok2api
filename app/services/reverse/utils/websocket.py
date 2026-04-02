"""
WebSocket helpers for reverse interfaces.
"""

from __future__ import annotations

import ssl
from typing import Any, Awaitable, Callable, Mapping, Optional
from urllib.parse import urlparse

import aiohttp
import certifi
from aiohttp_socks import ProxyConnector

from app.core.logger import logger
from app.services.config import get_config
from app.services.proxy.models import ProxyLease


def _default_ssl_context() -> ssl.SSLContext:
    context = ssl.create_default_context()
    context.load_verify_locations(certifi.where())
    return context


def _normalize_socks_proxy(proxy_url: str) -> tuple[str, Optional[bool]]:
    scheme = urlparse(proxy_url).scheme.lower()
    rdns: Optional[bool] = None
    base_scheme = scheme

    if scheme == "socks5h":
        base_scheme = "socks5"
        rdns = True
    elif scheme == "socks4a":
        base_scheme = "socks4"
        rdns = True

    if base_scheme != scheme:
        proxy_url = proxy_url.replace(f"{scheme}://", f"{base_scheme}://", 1)

    return proxy_url, rdns


def resolve_proxy(
    proxy_url: Optional[str] = None,
    ssl_context: ssl.SSLContext = _default_ssl_context(),
) -> tuple[aiohttp.BaseConnector, Optional[str]]:
    if not proxy_url:
        return aiohttp.TCPConnector(ssl=ssl_context), None

    scheme = urlparse(proxy_url).scheme.lower()
    if scheme.startswith("socks"):
        normalized, rdns = _normalize_socks_proxy(proxy_url)
        logger.info(f"Using SOCKS proxy: {proxy_url}")
        try:
            if rdns is not None:
                return (
                    ProxyConnector.from_url(normalized, rdns=rdns, ssl=ssl_context),
                    None,
                )
        except TypeError:
            return ProxyConnector.from_url(normalized, ssl=ssl_context), None
        return ProxyConnector.from_url(normalized, ssl=ssl_context), None

    logger.info(f"Using HTTP proxy: {proxy_url}")
    return aiohttp.TCPConnector(ssl=ssl_context), proxy_url


class WebSocketConnection:
    """WebSocket connection wrapper."""

    def __init__(
        self,
        session: aiohttp.ClientSession,
        ws: aiohttp.ClientWebSocketResponse,
        *,
        on_close: Callable[[], Awaitable[None]] | None = None,
    ) -> None:
        self.session = session
        self.ws = ws
        self._on_close = on_close

    async def close(self) -> None:
        if not self.ws.closed:
            await self.ws.close()
        await self.session.close()
        if self._on_close is not None:
            await self._on_close()
            self._on_close = None

    async def __aenter__(self) -> aiohttp.ClientWebSocketResponse:
        return self.ws

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.close()


class WebSocketClient:
    """WebSocket client with proxy support."""

    def __init__(self, proxy: Optional[str] = None) -> None:
        self._proxy_override = proxy
        self._ssl_context = _default_ssl_context()

    async def connect(
        self,
        url: str,
        headers: Optional[Mapping[str, str]] = None,
        timeout: Optional[float] = None,
        ws_kwargs: Optional[Mapping[str, object]] = None,
        *,
        lease: ProxyLease | None = None,
        on_close: Callable[[], Awaitable[None]] | None = None,
    ) -> WebSocketConnection:
        proxy_url = self._proxy_override or (lease.proxy_url if lease is not None else "")
        connector, resolved_proxy = resolve_proxy(proxy_url, self._ssl_context)
        logger.debug(
            "WebSocket connect: proxy_url={} resolved_proxy={} connector={}",
            proxy_url,
            resolved_proxy,
            type(connector).__name__,
        )

        total_timeout = (
            float(timeout)
            if timeout is not None
            else float(get_config("voice.timeout") or 120)
        )
        client_timeout = aiohttp.ClientTimeout(total=total_timeout)
        session = aiohttp.ClientSession(connector=connector, timeout=client_timeout)
        try:
            extra_kwargs: dict[str, Any] = dict(ws_kwargs or {})
            skip_proxy_ssl = bool(get_config("proxy.skip_proxy_ssl_verify")) and bool(proxy_url)
            if skip_proxy_ssl and urlparse(proxy_url).scheme.lower() == "https":
                proxy_ssl_context = ssl.create_default_context()
                proxy_ssl_context.check_hostname = False
                proxy_ssl_context.verify_mode = ssl.CERT_NONE
                try:
                    ws = await session.ws_connect(
                        url,
                        headers=headers,
                        proxy=resolved_proxy,
                        ssl=self._ssl_context,
                        proxy_ssl=proxy_ssl_context,
                        **extra_kwargs,
                    )
                except TypeError:
                    logger.warning(
                        "proxy.skip_proxy_ssl_verify is enabled, but aiohttp does not support proxy_ssl; keeping proxy SSL verification enabled"
                    )
                    ws = await session.ws_connect(
                        url,
                        headers=headers,
                        proxy=resolved_proxy,
                        ssl=self._ssl_context,
                        **extra_kwargs,
                    )
            else:
                ws = await session.ws_connect(
                    url,
                    headers=headers,
                    proxy=resolved_proxy,
                    ssl=self._ssl_context,
                    **extra_kwargs,
                )
            return WebSocketConnection(session, ws, on_close=on_close)
        except Exception:
            await session.close()
            raise


__all__ = ["WebSocketClient", "WebSocketConnection", "resolve_proxy"]
