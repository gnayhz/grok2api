"""
Shared proxy-domain helpers for reverse interfaces.
"""

from __future__ import annotations

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.proxy.feedback import build_feedback, classify_status_code
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease
from app.services.proxy.service import ProxyService


def get_upstream_status(error: Exception) -> int | None:
    if isinstance(error, UpstreamException):
        details = error.details if isinstance(error.details, dict) else {}
        status = details.get("status")
        if status is not None:
            return int(status)
        status = getattr(error, "status_code", None)
        return int(status) if status is not None else None
    status = getattr(error, "status", None)
    return int(status) if status is not None else None


def classify_proxy_error(error: Exception, status_code: int | None) -> ProxyFeedbackKind:
    if status_code is not None and isinstance(error, UpstreamException):
        return classify_status_code(status_code)
    return ProxyFeedbackKind.TRANSPORT_ERROR


async def report_proxy_lease(
    proxy_service: ProxyService,
    lease: ProxyLease | None,
    *,
    label: str,
    kind: ProxyFeedbackKind,
    status_code: int | None = None,
    reason: str = "",
    retry_after_ms: int | None = None,
) -> None:
    if lease is None:
        return
    try:
        await proxy_service.report(
            lease.lease_id,
            build_feedback(
                kind,
                status_code=status_code,
                reason=reason,
                retry_after_ms=retry_after_ms,
            ),
        )
    except Exception as error:
        logger.debug("{} proxy report failed: {}", label, error)


async def release_proxy_lease(
    proxy_service: ProxyService,
    lease: ProxyLease | None,
    *,
    label: str,
) -> None:
    if lease is None:
        return
    try:
        await proxy_service.release(lease.lease_id)
    except Exception as error:
        logger.debug("{} proxy release failed: {}", label, error)


__all__ = [
    "classify_proxy_error",
    "get_upstream_status",
    "release_proxy_lease",
    "report_proxy_lease",
]
