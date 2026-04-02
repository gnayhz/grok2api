"""
Reverse interface: download asset.
"""

import urllib.parse
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.account.token_service import TokenService
from app.services.config import get_config
from app.services.proxy.feedback import build_feedback, classify_status_code
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease, ProxyScope, RequestKind
from app.services.proxy.service import get_proxy_service
from app.services.proxy.session import build_http_proxies, build_session_kwargs
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.retry import retry_on_status

DOWNLOAD_API = "https://assets.grok.com"

_CONTENT_TYPES = {
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".png": "image/png",
    ".webp": "image/webp",
    ".mp4": "video/mp4",
    ".webm": "video/webm",
}


class AssetsDownloadReverse:
    """assets.grok.com/{path} reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str, file_path: str) -> Any:
        """Download asset from Grok."""
        try:
            parsed = urlparse(file_path)
            origin = "https://assets.grok.com"
            referer = "https://grok.com/"
            if parsed.scheme and parsed.netloc:
                url = file_path
                request_path = parsed.path or "/"
                if parsed.query:
                    request_path = f"{request_path}?{parsed.query}"
                origin = f"{parsed.scheme}://{parsed.netloc}"
                referer = f"{origin}/"
            else:
                if not file_path.startswith("/"):
                    file_path = f"/{file_path}"
                request_path = file_path
                url = f"{DOWNLOAD_API}{file_path}"

            content_type = _CONTENT_TYPES.get(
                Path(urllib.parse.urlparse(request_path).path).suffix.lower()
            )
            timeout = get_config("asset.download_timeout")
            default_browser = get_config("proxy.browser")
            proxy_service = get_proxy_service()
            active_lease: ProxyLease | None = None

            async def _report_active_lease(
                kind: ProxyFeedbackKind,
                *,
                status_code: int | None = None,
                reason: str = "",
                retry_after_ms: int | None = None,
            ) -> None:
                nonlocal active_lease
                if active_lease is None:
                    return
                try:
                    await proxy_service.report(
                        active_lease.lease_id,
                        build_feedback(
                            kind,
                            status_code=status_code,
                            reason=reason,
                            retry_after_ms=retry_after_ms,
                        ),
                    )
                except Exception as error:
                    logger.debug("AssetsDownloadReverse proxy report failed: {}", error)
                finally:
                    active_lease = None

            async def _do_request():
                nonlocal active_lease
                active_lease = await proxy_service.acquire(
                    scope=ProxyScope.ASSET,
                    request_kind=RequestKind.HTTP,
                )
                if active_lease is None:
                    raise UpstreamException(
                        message="AssetsDownloadReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    content_type=content_type,
                    origin=origin,
                    referer=referer,
                    lease=active_lease,
                )
                headers["Cache-Control"] = "no-cache"
                headers["Pragma"] = "no-cache"
                headers["Priority"] = "u=0, i"
                headers["Sec-Fetch-Mode"] = "navigate"
                headers["Sec-Fetch-User"] = "?1"
                headers["Upgrade-Insecure-Requests"] = "1"
                response = await session.get(
                    url,
                    headers=headers,
                    allow_redirects=True,
                    stream=True,
                    **build_session_kwargs(
                        lease=active_lease,
                        browser_override=default_browser,
                        kwargs={
                            "proxies": build_http_proxies(active_lease.proxy_url),
                            "timeout": timeout,
                        },
                    ),
                )

                if response.status_code != 200:
                    logger.error(
                        f"AssetsDownloadReverse: Download failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"AssetsDownloadReverse: Download failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "headers": dict(response.headers or {}),
                        },
                    )

                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                kind = (
                    classify_status_code(status_code)
                    if isinstance(error, UpstreamException)
                    else ProxyFeedbackKind.TRANSPORT_ERROR
                )
                await _report_active_lease(
                    kind,
                    status_code=status_code,
                    reason=f"retry_attempt_{attempt}",
                    retry_after_ms=int(delay * 1000),
                )

            response = await retry_on_status(_do_request, on_retry=_on_retry)
            await _report_active_lease(ProxyFeedbackKind.SUCCESS, status_code=200)
            return response

        except Exception as e:
            if isinstance(e, UpstreamException):
                status = None
                if e.details and "status" in e.details:
                    status = e.details["status"]
                if status is None:
                    status = getattr(e, "status_code", None)
                if status is not None:
                    await _report_active_lease(
                        classify_status_code(status),
                        status_code=status,
                        reason="request_failed",
                    )
                if status == 401:
                    try:
                        await TokenService.record_fail(token, status, "assets_download_auth_failed")
                    except Exception:
                        pass
                raise

            logger.error(
                f"AssetsDownloadReverse: Download failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            await _report_active_lease(
                ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"AssetsDownloadReverse: Download failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["AssetsDownloadReverse"]
