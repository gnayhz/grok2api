"""
Reverse interface: rate limits.
"""

from __future__ import annotations

import traceback
from typing import Any

import orjson
from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.config import get_config
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease, ProxyScope, RequestKind
from app.services.proxy.service import get_proxy_service
from app.services.proxy.session import build_http_proxies, build_session_kwargs
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.proxy import (
    get_upstream_status,
    report_proxy_lease,
)
from app.services.reverse.utils.retry import retry_on_status

RATE_LIMITS_API = "https://grok.com/rest/rate-limits"


class RateLimitsReverse:
    """/rest/rate-limits reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> Any:
        """Fetch rate limits from Grok."""
        try:
            payload = {
                "requestKind": "DEFAULT",
                "modelName": "grok-4-1-thinking-1129",
            }
            timeout = get_config("usage.timeout")
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
                        message="RateLimitsReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    content_type="application/json",
                    origin="https://grok.com",
                    referer="https://grok.com/",
                    lease=active_lease,
                )
                response = await session.post(
                    RATE_LIMITS_API,
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
                    try:
                        resp_text = response.text
                    except Exception:
                        resp_text = "N/A"

                    is_token_expired = False
                    server_header = response.headers.get("Server", "").lower()
                    content_type = response.headers.get("Content-Type", "").lower()

                    is_cloudflare = "challenge-platform" in resp_text
                    if "cloudflare" in server_header and "application/json" not in content_type:
                        is_cloudflare = True

                    if response.status_code == 401 and "application/json" in content_type:
                        body_lower = resp_text.lower()
                        auth_error_keywords = [
                            "unauthorized",
                            "not logged in",
                            "unauthenticated",
                            "bad-credentials",
                        ]
                        if any(keyword in body_lower for keyword in auth_error_keywords):
                            is_token_expired = True

                    logger.error(
                        "RateLimitsReverse: Request failed, status={}, is_token_expired={}, is_cloudflare={}, Body: {}",
                        response.status_code,
                        is_token_expired,
                        is_cloudflare,
                        resp_text[:300],
                        extra={"error_type": "UpstreamException"},
                    )

                    raise UpstreamException(
                        message=f"RateLimitsReverse: Request failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "body": resp_text,
                            "is_token_expired": is_token_expired,
                            "is_cloudflare": is_cloudflare,
                            "headers": dict(response.headers or {}),
                        },
                    )

                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                nonlocal active_lease
                kind = (
                    ProxyFeedbackKind.TRANSPORT_ERROR
                    if not isinstance(error, UpstreamException)
                    else ProxyFeedbackKind.RATE_LIMITED
                    if status_code == 429
                    else ProxyFeedbackKind.CHALLENGE
                    if status_code == 403
                    else ProxyFeedbackKind.UNAUTHORIZED
                    if status_code == 401
                    else ProxyFeedbackKind.UPSTREAM_5XX
                    if status_code >= 500
                    else ProxyFeedbackKind.FORBIDDEN
                )
                await report_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="RateLimitsReverse",
                    kind=kind,
                    status_code=status_code,
                    reason=f"retry_attempt_{attempt}",
                    retry_after_ms=int(delay * 1000),
                )
                active_lease = None

            response = await retry_on_status(_do_request, on_retry=_on_retry)
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="RateLimitsReverse",
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
                        label="RateLimitsReverse",
                        kind=ProxyFeedbackKind.RATE_LIMITED
                        if status == 429
                        else ProxyFeedbackKind.CHALLENGE
                        if status == 403
                        else ProxyFeedbackKind.UNAUTHORIZED
                        if status == 401
                        else ProxyFeedbackKind.UPSTREAM_5XX
                        if status >= 500
                        else ProxyFeedbackKind.FORBIDDEN,
                        status_code=status,
                        reason="request_failed",
                    )
                    active_lease = None
                logger.debug(f"RateLimitsReverse: Upstream error caught: {str(e)}, status={status}")
                raise

            error_details = traceback.format_exc()
            logger.error(
                f"RateLimitsReverse: Unexpected error, {type(e).__name__}: {str(e)}\n{error_details}"
            )
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="RateLimitsReverse",
                kind=ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"RateLimitsReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e), "traceback": error_details},
            )


__all__ = ["RateLimitsReverse"]
