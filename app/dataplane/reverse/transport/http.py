"""HTTP transport for reverse-proxy requests.

Wraps curl_cffi AsyncSession; handles proxy selection, header building,
retry-on-reset, and timeout.
"""

from __future__ import annotations

import asyncio
from typing import Any, AsyncGenerator

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import UpstreamError
from app.control.proxy.models import ProxyLease
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs


async def post_stream(
    url:     str,
    token:   str,
    payload: bytes,
    *,
    lease:        ProxyLease | None = None,
    timeout_s:    float             = 120.0,
    content_type: str               = "application/json",
    origin:       str               = "https://grok.com",
    referer:      str               = "https://grok.com/",
) -> AsyncGenerator[str, None]:
    """POST *url* and yield SSE lines from the streaming response.

    Raises ``UpstreamError`` on non-200 status.
    """
    headers = build_http_headers(
        token,
        content_type = content_type,
        origin       = origin,
        referer      = referer,
        lease        = lease,
    )
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            url,
            headers = headers,
            data    = payload,
            timeout = timeout_s,
            stream  = True,
        )

        if response.status_code != 200:
            try:
                body = (await response.aread()).decode("utf-8", "replace")[:400]
            except Exception:
                body = ""
            logger.error(
                "HTTP POST failed: url={} status={} body={}",
                url, response.status_code, body,
            )
            raise UpstreamError(
                f"Upstream returned {response.status_code}",
                status = response.status_code,
                body   = body,
            )

        async def _lines() -> AsyncGenerator[str, None]:
            try:
                async for line in response.aiter_lines():
                    yield line
            finally:
                try:
                    await session.close()
                except Exception:
                    pass

        return _lines()


async def post_json(
    url:     str,
    token:   str,
    payload: bytes,
    *,
    lease:        ProxyLease | None = None,
    timeout_s:    float             = 30.0,
    content_type: str               = "application/json",
    origin:       str               = "https://grok.com",
    referer:      str               = "https://grok.com/",
) -> dict:
    """POST *url* and return parsed JSON response body."""
    headers = build_http_headers(
        token,
        content_type = content_type,
        origin       = origin,
        referer      = referer,
        lease        = lease,
    )
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            url,
            headers = headers,
            data    = payload,
            timeout = timeout_s,
        )

        body_bytes = await response.aread()
        if response.status_code not in (200, 201, 204):
            body_text = body_bytes.decode("utf-8", "replace")[:400]
            logger.error(
                "HTTP POST JSON failed: url={} status={} body={}",
                url, response.status_code, body_text,
            )
            raise UpstreamError(
                f"Upstream returned {response.status_code}",
                status = response.status_code,
                body   = body_text,
            )

        if not body_bytes.strip():
            return {}
        import orjson
        return orjson.loads(body_bytes)


async def get_json(
    url:   str,
    token: str,
    *,
    params:    dict | None       = None,
    lease:     ProxyLease | None = None,
    timeout_s: float             = 30.0,
    origin:    str               = "https://grok.com",
    referer:   str               = "https://grok.com/",
) -> dict:
    """GET *url* and return parsed JSON response body."""
    headers = build_http_headers(
        token,
        content_type = "application/json",
        origin       = origin,
        referer      = referer,
        lease        = lease,
    )
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.get(
            url,
            headers = headers,
            params  = params,
            timeout = timeout_s,
        )

        body_bytes = await response.aread()
        if response.status_code != 200:
            body_text = body_bytes.decode("utf-8", "replace")[:400]
            logger.error(
                "HTTP GET failed: url={} status={} body={}",
                url, response.status_code, body_text,
            )
            raise UpstreamError(
                f"Upstream returned {response.status_code}",
                status = response.status_code,
                body   = body_text,
            )

        import orjson
        return orjson.loads(body_bytes)


async def delete_json(
    url:   str,
    token: str,
    *,
    lease:     ProxyLease | None = None,
    timeout_s: float             = 30.0,
    origin:    str               = "https://grok.com",
    referer:   str               = "https://grok.com/",
) -> dict:
    """DELETE *url* and return parsed JSON response body (may be empty → {})."""
    headers = build_http_headers(
        token,
        content_type = "application/json",
        origin       = origin,
        referer      = referer,
        lease        = lease,
    )
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.delete(
            url,
            headers = headers,
            timeout = timeout_s,
        )

        body_bytes = await response.aread()
        if response.status_code not in (200, 204):
            body_text = body_bytes.decode("utf-8", "replace")[:400]
            logger.error(
                "HTTP DELETE failed: url={} status={} body={}",
                url, response.status_code, body_text,
            )
            raise UpstreamError(
                f"Upstream returned {response.status_code}",
                status = response.status_code,
                body   = body_text,
            )

        if not body_bytes.strip():
            return {}
        import orjson
        return orjson.loads(body_bytes)


async def get_bytes_stream(
    url:   str,
    token: str,
    *,
    lease:        ProxyLease | None = None,
    timeout_s:    float             = 120.0,
    origin:       str               = "https://assets.grok.com",
    referer:      str               = "https://grok.com/",
    extra_headers: dict | None      = None,
) -> AsyncGenerator[bytes, None]:
    """GET *url* and yield raw bytes chunks from the streaming response.

    Raises ``UpstreamError`` on non-200 status.
    """
    headers = build_http_headers(
        token,
        content_type = None,
        origin       = origin,
        referer      = referer,
        lease        = lease,
    )
    if extra_headers:
        headers.update(extra_headers)
    kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.get(
            url,
            headers         = headers,
            timeout         = timeout_s,
            stream          = True,
            allow_redirects = True,
        )

        if response.status_code != 200:
            try:
                body = (await response.aread()).decode("utf-8", "replace")[:400]
            except Exception:
                body = ""
            logger.error(
                "HTTP GET stream failed: url={} status={} body={}",
                url, response.status_code, body,
            )
            raise UpstreamError(
                f"Upstream returned {response.status_code}",
                status = response.status_code,
                body   = body,
            )

        async def _chunks() -> AsyncGenerator[bytes, None]:
            try:
                async for chunk in response.aiter_content():
                    if chunk:
                        yield chunk
            finally:
                try:
                    await session.close()
                except Exception:
                    pass

        return _chunks()


__all__ = ["post_stream", "post_json", "get_json", "delete_json", "get_bytes_stream"]
