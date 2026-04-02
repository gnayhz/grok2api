"""
Reverse interface: media post create.
"""

from __future__ import annotations

from typing import Any

import orjson
from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.account.token_service import TokenService
from app.services.config import get_config
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease, ProxyScope, RequestKind
from app.services.proxy.service import get_proxy_service
from app.services.proxy.session import build_http_proxies, build_session_kwargs
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.proxy import (
    classify_proxy_error,
    get_upstream_status,
    report_proxy_lease,
)
from app.services.reverse.utils.retry import retry_on_status

MEDIA_POST_API = "https://grok.com/rest/media/post/create"


class MediaPostReverse:
    """/rest/media/post/create reverse interface."""

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        mediaType: str,
        mediaUrl: str,
        prompt: str = "",
    ) -> Any:
        """Create media post in Grok."""
        try:
            payload = {"mediaType": mediaType}
            if mediaUrl:
                payload["mediaUrl"] = mediaUrl
            if prompt:
                payload["prompt"] = prompt

            timeout = get_config("video.timeout")
            default_browser = get_config("proxy.browser")
            proxy_service = get_proxy_service()
            active_lease: ProxyLease | None = None

            async def _do_request():
                nonlocal active_lease
                active_lease = await proxy_service.acquire(
                    scope=ProxyScope.APP,
                    request_kind=RequestKind.HTTP,
                )
                if active_lease is None:
                    raise UpstreamException(
                        message="MediaPostReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    content_type="application/json",
                    origin="https://grok.com",
                    referer="https://grok.com",
                    lease=active_lease,
                )
                response = await session.post(
                    MEDIA_POST_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    **build_session_kwargs(
                        lease=active_lease,
                        browser_override=default_browser,
                        kwargs={
                            "timeout": timeout,
                            "proxies": build_http_proxies(active_lease.proxy_url),
                        },
                    ),
                )

                if response.status_code != 200:
                    content = ""
                    try:
                        content = await response.text()
                    except Exception:
                        pass
                    logger.error(
                        f"MediaPostReverse: Media post create failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"MediaPostReverse: Media post create failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "body": content,
                            "headers": dict(response.headers or {}),
                        },
                    )

                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                nonlocal active_lease
                await report_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="MediaPostReverse",
                    kind=classify_proxy_error(error, status_code),
                    status_code=status_code,
                    reason=f"retry_attempt_{attempt}",
                    retry_after_ms=int(delay * 1000),
                )
                active_lease = None

            response = await retry_on_status(_do_request, on_retry=_on_retry)
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="MediaPostReverse",
                kind=ProxyFeedbackKind.SUCCESS,
                status_code=200,
            )
            active_lease = None
            return response

        except Exception as e:
            if isinstance(e, UpstreamException):
                status = get_upstream_status(e)
                if status is not None:
                    await report_proxy_lease(
                        proxy_service,
                        active_lease,
                        label="MediaPostReverse",
                        kind=classify_proxy_error(e, status),
                        status_code=status,
                        reason="request_failed",
                    )
                    active_lease = None
                if status == 401:
                    try:
                        await TokenService.record_fail(token, status, "media_post_auth_failed")
                    except Exception:
                        pass
                raise

            logger.error(
                f"MediaPostReverse: Media post create failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="MediaPostReverse",
                kind=ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"MediaPostReverse: Media post create failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["MediaPostReverse"]
