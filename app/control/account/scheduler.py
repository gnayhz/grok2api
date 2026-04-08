"""Background scheduler for periodic account quota refresh.

Runs one independent loop per pool type (basic / super / heavy), each with
its own configurable interval read from:

    account.refresh.basic_interval_sec  (default 36000 — 10 h)
    account.refresh.super_interval_sec  (default  7200 —  2 h)
    account.refresh.heavy_interval_sec  (default  7200 —  2 h)

Falls back to the legacy ``account.refresh.interval_sec`` key if the
pool-specific key is absent (backward compatibility with existing configs).
"""

import asyncio

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from .refresh import AccountRefreshService

# Pool → (config key, built-in default seconds)
_POOL_CONFIG: dict[str, tuple[str, int]] = {
    "basic": ("account.refresh.basic_interval_sec", 36_000),
    "super": ("account.refresh.super_interval_sec",  7_200),
    "heavy": ("account.refresh.heavy_interval_sec",  7_200),
}


def _interval(pool: str) -> int:
    key, default = _POOL_CONFIG[pool]
    # Pool-specific key takes precedence; legacy key as second fallback.
    v = get_config(key, None)
    if v is None:
        v = get_config("account.refresh.interval_sec", None)
    return int(v) if v is not None else default


class AccountRefreshScheduler:
    """Runs one refresh loop per pool type at pool-specific intervals.

    Lifecycle:  ``start()`` → loops run in background → ``stop()`` to cancel.
    """

    def __init__(self, refresh_service: AccountRefreshService) -> None:
        self._service = refresh_service
        self._tasks:  list[asyncio.Task] = []
        self._stop    = asyncio.Event()

    def start(self) -> None:
        if self._tasks and not all(t.done() for t in self._tasks):
            return
        self._stop.clear()
        self._tasks = [
            asyncio.create_task(self._loop(pool), name=f"account-refresh-{pool}")
            for pool in _POOL_CONFIG
        ]
        logger.info(
            "AccountRefreshScheduler started — basic={}s super={}s heavy={}s",
            _interval("basic"), _interval("super"), _interval("heavy"),
        )

    def stop(self) -> None:
        self._stop.set()
        for t in self._tasks:
            if not t.done():
                t.cancel()
        logger.info("AccountRefreshScheduler stopped.")

    async def _loop(self, pool: str) -> None:
        while not self._stop.is_set():
            interval = _interval(pool)
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=float(interval))
                break  # stop event fired
            except asyncio.TimeoutError:
                pass

            if self._stop.is_set():
                break

            try:
                result = await self._service.refresh_scheduled(pool=pool)
                logger.info(
                    "Account refresh [{}]: checked={} refreshed={} recovered={} failed={}",
                    pool, result.checked, result.refreshed, result.recovered, result.failed,
                )
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.error("Account refresh [{}] error: {}", pool, exc)


# ---------------------------------------------------------------------------
# Module-level singleton
# ---------------------------------------------------------------------------

_scheduler: AccountRefreshScheduler | None = None


def get_account_refresh_scheduler(
    refresh_service: AccountRefreshService,
) -> AccountRefreshScheduler:
    global _scheduler
    if _scheduler is None:
        _scheduler = AccountRefreshScheduler(refresh_service)
    return _scheduler


__all__ = ["AccountRefreshScheduler", "get_account_refresh_scheduler"]
