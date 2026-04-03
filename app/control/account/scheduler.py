"""Background scheduler for periodic account quota refresh."""

from __future__ import annotations

import asyncio

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from .refresh import AccountRefreshService


class AccountRefreshScheduler:
    """Runs ``AccountRefreshService.refresh_scheduled`` on a fixed interval.

    Lifecycle:  ``start()`` → runs in background → ``stop()`` to cancel.
    """

    def __init__(self, refresh_service: AccountRefreshService) -> None:
        self._service = refresh_service
        self._task:   asyncio.Task | None = None
        self._stop    = asyncio.Event()

    def start(self) -> None:
        if self._task and not self._task.done():
            return
        self._stop.clear()
        self._task = asyncio.create_task(self._loop(), name="account-refresh-scheduler")
        logger.info("AccountRefreshScheduler started.")

    def stop(self) -> None:
        self._stop.set()
        if self._task and not self._task.done():
            self._task.cancel()
        logger.info("AccountRefreshScheduler stopped.")

    async def _loop(self) -> None:
        while not self._stop.is_set():
            interval = int(get_config("account.refresh.interval_sec", 300))
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=float(interval))
                break  # stop was set
            except asyncio.TimeoutError:
                pass

            if self._stop.is_set():
                break

            try:
                result = await self._service.refresh_scheduled()
                logger.info(
                    "Account refresh complete: checked={} refreshed={} recovered={} "
                    "expired={} disabled={} failed={}",
                    result.checked, result.refreshed, result.recovered,
                    result.expired, result.disabled, result.failed,
                )
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.error("Account refresh error: {}", exc)


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
