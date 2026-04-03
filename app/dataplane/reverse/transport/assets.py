"""Asset management transport — list, delete, download.

All functions acquire a proxy lease internally, execute the upstream call,
give feedback, and return results to the caller.
"""

from __future__ import annotations

from typing import Any, AsyncGenerator, Dict, Optional

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import UpstreamError
from app.control.proxy.models import ProxyFeedback, ProxyFeedbackKind, ProxyScope, RequestKind
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.reverse.protocol.xai_assets import (
    ASSETS_LIST_URL,
    asset_delete_url,
    infer_content_type,
    resolve_download_url,
)
from app.dataplane.reverse.transport.http import (
    delete_json,
    get_bytes_stream,
    get_json,
)


# ------------------------------------------------------------------
# List assets
# ------------------------------------------------------------------

async def list_assets(
    token:  str,
    params: Optional[Dict[str, Any]] = None,
) -> dict:
    """GET /rest/assets and return the JSON response.

    Args:
        token:  SSO session token.
        params: Optional query parameters (e.g. ``{"cursor": "...", "limit": 50}``).
    """
    cfg       = get_config()
    timeout_s = cfg.get_float("asset.list_timeout", 30.0)

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire(scope=ProxyScope.ASSET, kind=RequestKind.HTTP)

    try:
        result = await get_json(
            ASSETS_LIST_URL,
            token,
            params    = params,
            lease     = lease,
            timeout_s = timeout_s,
            origin    = "https://grok.com",
            referer   = "https://grok.com/files",
        )
    except UpstreamError as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(
                kind        = ProxyFeedbackKind.UPSTREAM_5XX if (exc.status or 0) >= 500
                              else ProxyFeedbackKind.FORBIDDEN,
                status_code = exc.status or 502,
            ),
        )
        raise
    except Exception as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR),
        )
        raise UpstreamError(f"list_assets: transport error: {exc}") from exc

    await proxy.feedback(
        lease,
        ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
    )
    return result


# ------------------------------------------------------------------
# Delete asset
# ------------------------------------------------------------------

async def delete_asset(token: str, asset_id: str) -> dict:
    """DELETE /rest/assets-metadata/{asset_id} and return the JSON body (may be {})."""
    cfg       = get_config()
    timeout_s = cfg.get_float("asset.delete_timeout", 30.0)

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire(scope=ProxyScope.ASSET, kind=RequestKind.HTTP)

    try:
        result = await delete_json(
            asset_delete_url(asset_id),
            token,
            lease     = lease,
            timeout_s = timeout_s,
            origin    = "https://grok.com",
            referer   = "https://grok.com/files",
        )
    except UpstreamError as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(
                kind        = ProxyFeedbackKind.UPSTREAM_5XX if (exc.status or 0) >= 500
                              else ProxyFeedbackKind.FORBIDDEN,
                status_code = exc.status or 502,
            ),
        )
        raise
    except Exception as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR),
        )
        raise UpstreamError(f"delete_asset: transport error: {exc}") from exc

    await proxy.feedback(
        lease,
        ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
    )
    logger.debug("delete_asset: asset_id={} ok", asset_id)
    return result


# ------------------------------------------------------------------
# Download asset (streaming)
# ------------------------------------------------------------------

async def download_asset(
    token:     str,
    file_path: str,
) -> tuple[AsyncGenerator[bytes, None], Optional[str]]:
    """Stream asset bytes from assets.grok.com.

    Args:
        token:     SSO session token.
        file_path: URL, absolute, or relative path of the asset.

    Returns:
        ``(byte_stream, content_type)`` — an async generator of raw bytes and
        a best-guess MIME type (may be ``None`` if unknown).
    """
    cfg       = get_config()
    timeout_s = cfg.get_float("asset.download_timeout", 120.0)

    url, origin, referer = resolve_download_url(file_path)
    content_type = infer_content_type(url)

    extra: Dict[str, str] = {
        "Cache-Control":            "no-cache",
        "Pragma":                   "no-cache",
        "Priority":                 "u=0, i",
        "Sec-Fetch-Mode":           "navigate",
        "Sec-Fetch-User":           "?1",
        "Upgrade-Insecure-Requests": "1",
    }
    if content_type:
        extra["Content-Type"] = content_type

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire(scope=ProxyScope.ASSET, kind=RequestKind.HTTP)

    try:
        stream = await get_bytes_stream(
            url,
            token,
            lease         = lease,
            timeout_s     = timeout_s,
            origin        = origin,
            referer       = referer,
            extra_headers = extra,
        )
    except UpstreamError as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(
                kind        = ProxyFeedbackKind.UPSTREAM_5XX if (exc.status or 0) >= 500
                              else ProxyFeedbackKind.FORBIDDEN,
                status_code = exc.status or 502,
            ),
        )
        raise
    except Exception as exc:
        await proxy.feedback(
            lease,
            ProxyFeedback(kind=ProxyFeedbackKind.TRANSPORT_ERROR),
        )
        raise UpstreamError(f"download_asset: transport error: {exc}") from exc

    # Feedback is deferred: the caller drains the stream and must not rely
    # on the lease being reported here.  We report success eagerly since
    # the transport already confirmed 200.
    await proxy.feedback(
        lease,
        ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200),
    )
    return stream, content_type


__all__ = ["list_assets", "delete_asset", "download_asset"]
