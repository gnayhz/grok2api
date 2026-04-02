"""
Feedback classification helpers for the proxy domain.
"""

from __future__ import annotations

import time
from app.services.proxy.models import ProxyFeedback, ProxyFeedbackKind


def _now_ms() -> int:
    return int(time.time() * 1000)


def build_feedback(
    kind: ProxyFeedbackKind,
    *,
    status_code: int | None = None,
    reason: str = "",
    retry_after_ms: int | None = None,
) -> ProxyFeedback:
    return ProxyFeedback(
        kind=kind,
        status_code=status_code,
        reason=reason,
        at=_now_ms(),
        retry_after_ms=retry_after_ms,
    )


def classify_status_code(status_code: int) -> ProxyFeedbackKind:
    if status_code == 401:
        return ProxyFeedbackKind.UNAUTHORIZED
    if status_code == 429:
        return ProxyFeedbackKind.RATE_LIMITED
    if status_code == 403:
        return ProxyFeedbackKind.CHALLENGE
    if status_code >= 500:
        return ProxyFeedbackKind.UPSTREAM_5XX
    return ProxyFeedbackKind.FORBIDDEN


__all__ = ["build_feedback", "classify_status_code"]
