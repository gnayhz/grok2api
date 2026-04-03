"""Default quota windows applied when upstream data is unavailable."""

from __future__ import annotations

from .enums import QuotaSource
from .models import AccountQuotaSet, QuotaWindow


def _w(remaining: int, total: int, window_seconds: int) -> QuotaWindow:
    return QuotaWindow(
        remaining      = remaining,
        total          = total,
        window_seconds = window_seconds,
        reset_at       = None,
        synced_at      = None,
        source         = QuotaSource.DEFAULT,
    )


# ---------------------------------------------------------------------------
# Basic-pool defaults
# (static window — no API sync required)
# ---------------------------------------------------------------------------
BASIC_QUOTA_DEFAULTS = AccountQuotaSet(
    auto   = _w(20,  20,  72_000),   # 20 queries / 20 h
    fast   = _w(60,  60,  72_000),   # 60 queries / 20 h
    expert = _w(8,   8,   36_000),   # 8  queries / 10 h
)

# ---------------------------------------------------------------------------
# Super-pool defaults
# (used only when upstream API call fails; real values fetched on import)
# ---------------------------------------------------------------------------
SUPER_QUOTA_DEFAULTS = AccountQuotaSet(
    auto   = _w(20,  20,  72_000),
    fast   = _w(60,  60,  72_000),
    expert = _w(8,   8,   36_000),
)


def default_quota_set(pool: str) -> AccountQuotaSet:
    """Return a fresh copy of the default quota set for *pool*."""
    src = SUPER_QUOTA_DEFAULTS if pool == "super" else BASIC_QUOTA_DEFAULTS
    return AccountQuotaSet(
        auto   = _w(src.auto.remaining,   src.auto.total,   src.auto.window_seconds),
        fast   = _w(src.fast.remaining,   src.fast.total,   src.fast.window_seconds),
        expert = _w(src.expert.remaining, src.expert.total, src.expert.window_seconds),
    )


__all__ = ["BASIC_QUOTA_DEFAULTS", "SUPER_QUOTA_DEFAULTS", "default_quota_set"]
