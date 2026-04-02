"""
Reverse interface: set birth date.
"""

from __future__ import annotations

import datetime
import random
from typing import Any

from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
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

SET_BIRTH_API = "https://grok.com/rest/auth/set-birth-date"


class SetBirthReverse:
    """/rest/auth/set-birth-date reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> Any:
        """Set birth date in Grok."""
        try:
            today = datetime.date.today()
            birth_year = today.year - random.randint(20, 48)
            birth_month = random.randint(1, 12)
            birth_day = random.randint(1, 28)
            hour = random.randint(0, 23)
            minute = random.randint(0, 59)
            second = random.randint(0, 59)
            microsecond = random.randint(0, 999)
            payload = {
                "birthDate": f"{birth_year:04d}-{birth_month:02d}-{birth_day:02d}"
                f"T{hour:02d}:{minute:02d}:{second:02d}.{microsecond:03d}Z"
            }

            timeout = get_config("nsfw.timeout")
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
                        message="SetBirthReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    content_type="application/json",
                    origin="https://grok.com",
                    referer="https://grok.com/?_s=home",
                    lease=active_lease,
                )
                response = await session.post(
                    SET_BIRTH_API,
                    headers=headers,
                    json=payload,
                    **build_session_kwargs(
                        lease=active_lease,
                        browser_override=default_browser,
                        kwargs={
                            "timeout": timeout,
                            "proxies": build_http_proxies(active_lease.proxy_url),
                        },
                    ),
                )

                if response.status_code not in (200, 204):
                    logger.error(
                        f"SetBirthReverse: Request failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"SetBirthReverse: Request failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "headers": dict(response.headers or {}),
                        },
                    )

                logger.debug(f"SetBirthReverse: Request successful, {response.status_code}")
                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                nonlocal active_lease
                await report_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="SetBirthReverse",
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
                label="SetBirthReverse",
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
                        label="SetBirthReverse",
                        kind=classify_proxy_error(e, status),
                        status_code=status,
                        reason="request_failed",
                    )
                    active_lease = None
                raise

            logger.error(
                f"SetBirthReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="SetBirthReverse",
                kind=ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"SetBirthReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["SetBirthReverse"]
