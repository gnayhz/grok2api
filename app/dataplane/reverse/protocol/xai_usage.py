"""XAI rate-limits API protocol — fetch live quota data per mode."""

import orjson

from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms


# ---------------------------------------------------------------------------
# Mode → request model name
# ---------------------------------------------------------------------------

# Each mode requires a separate POST request.
# The API returns a flat single-mode quota object per call.
_MODE_NAMES: dict[int, str] = {
    0: "auto",
    1: "fast",
    2: "expert",
    3: "heavy",
}

# Default window durations used as fallback when API call fails.
_DEFAULT_WINDOW_SECS: dict[int, int] = {
    0: 72_000,   # auto   — 20 h (basic) / 2 h (super/heavy, real value overrides)
    1: 72_000,   # fast   — 20 h (basic)
    2: 36_000,   # expert — 10 h (basic)
    3:  7_200,   # heavy  — 2 h  (heavy-pool only)
}


def _build_payload(mode_name: str) -> bytes:
    """Build rate-limits request payload: {"modelName": "fast"}"""
    return orjson.dumps({"modelName": mode_name})


# ---------------------------------------------------------------------------
# Response parser
# ---------------------------------------------------------------------------

def parse_rate_limits(body: dict) -> dict | None:
    """Parse flat rate-limits response.

    Expected format::

        {
            "windowSizeSeconds": 72000,
            "remainingQueries":  20,
            "totalQueries":      20,
            "lowEffortRateLimits":  null,
            "highEffortRateLimits": null
        }

    Returns a dict with keys ``remaining``, ``total``, ``window_seconds``
    or ``None`` if the required fields are absent.
    """
    remaining = body.get("remainingQueries")
    if remaining is None:
        return None
    total       = body.get("totalQueries")
    window_secs = body.get("windowSizeSeconds")
    return {
        "remaining":      int(remaining),
        "total":          int(total) if total is not None else int(remaining),
        "window_seconds": int(window_secs) if window_secs else 72_000,
    }


# ---------------------------------------------------------------------------
# QuotaWindow builder
# ---------------------------------------------------------------------------

def _to_quota_window(data: dict, synced_at: int) -> object:
    from app.control.account.models import QuotaWindow
    from app.control.account.enums import QuotaSource

    ws = data["window_seconds"]
    return QuotaWindow(
        remaining      = data["remaining"],
        total          = data["total"],
        window_seconds = ws,
        reset_at       = synced_at + ws * 1000,   # estimated end of current window
        synced_at      = synced_at,
        source         = QuotaSource.REAL,
    )


# ---------------------------------------------------------------------------
# HTTP fetch
# ---------------------------------------------------------------------------

async def _do_fetch(token: str, mode_name: str) -> dict:
    """POST the rate-limits endpoint for one mode and return parsed JSON body."""
    from app.dataplane.reverse.transport.http import post_json
    from app.dataplane.proxy import get_proxy_runtime
    from app.control.proxy.models import ProxyFeedback, ProxyFeedbackKind

    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()
    try:
        body = await post_json(
            "https://grok.com/rest/rate-limits",
            token,
            _build_payload(mode_name),
            lease     = lease,
            timeout_s = 20.0,
        )
        await proxy.feedback(lease, ProxyFeedback(kind=ProxyFeedbackKind.SUCCESS, status_code=200))
        return body
    except Exception as exc:
        status = getattr(exc, "status", None) or getattr(exc, "status_code", None)
        kind = (
            ProxyFeedbackKind.RATE_LIMITED  if status == 429 else
            ProxyFeedbackKind.CHALLENGE     if status == 403 else
            ProxyFeedbackKind.UNAUTHORIZED  if status == 401 else
            ProxyFeedbackKind.UPSTREAM_5XX  if status and status >= 500 else
            ProxyFeedbackKind.TRANSPORT_ERROR
        )
        await proxy.feedback(lease, ProxyFeedback(kind=kind, status_code=status))
        raise


async def _fetch_one(token: str, mode_id: int) -> object | None:
    """Fetch quota window for a single mode. Returns QuotaWindow or None."""
    mode_name = _MODE_NAMES.get(mode_id, "auto")
    try:
        body = await _do_fetch(token, mode_name)
    except Exception as exc:
        logger.debug("rate-limits fetch failed: token={}... mode={} err={}", token[:10], mode_name, exc)
        return None

    data = parse_rate_limits(body)
    if data is None:
        logger.debug("rate-limits empty response: token={}... mode={} body={}", token[:10], mode_name, body)
        return None

    return _to_quota_window(data, now_ms())


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

async def fetch_all_quotas(token: str) -> dict[int, object] | None:
    """Fetch quota windows for all four modes (auto / fast / expert / heavy) concurrently.

    Issues four requests in parallel.  Mode 3 (heavy) will silently return
    ``None`` for basic and super accounts — this is expected and harmless.
    Returns ``{mode_id: QuotaWindow}`` for every mode that responded
    successfully, or ``None`` if all four failed.
    """
    import asyncio
    results = await asyncio.gather(
        _fetch_one(token, 0),
        _fetch_one(token, 1),
        _fetch_one(token, 2),
        _fetch_one(token, 3),
        return_exceptions=False,
    )
    windows = {mode_id: win for mode_id, win in enumerate(results) if win is not None}
    return windows if windows else None


async def fetch_mode_quota(token: str, mode_id: int) -> object | None:
    """Fetch the quota window for a single mode. Returns QuotaWindow or None."""
    return await _fetch_one(token, mode_id)


__all__ = ["parse_rate_limits", "fetch_all_quotas", "fetch_mode_quota"]
