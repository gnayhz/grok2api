"""
Scheduler for periodic account cooling refresh.
"""

from __future__ import annotations

import asyncio
from typing import Optional

from app.core.logger import logger
from app.core.storage import StorageError, get_storage
from app.services.account.refresh import AccountRefreshPolicy, AccountRefreshService


class AccountRefreshScheduler:
    def __init__(
        self,
        refresh_service: AccountRefreshService,
        *,
        interval_seconds: Optional[int] = None,
        lock_timeout_sec: int = 1,
    ):
        self.refresh_service = refresh_service
        self.interval_seconds = interval_seconds or refresh_service.policy.scheduler_interval_sec
        self.lock_timeout_sec = max(1, lock_timeout_sec)
        self._task: Optional[asyncio.Task] = None
        self._running = False

    async def _refresh_loop(self) -> None:
        logger.info(
            "Account scheduler started: interval={}s",
            self.interval_seconds,
        )
        while self._running:
            try:
                storage = get_storage()
                try:
                    async with storage.acquire_lock(
                        "account_refresh_scheduler",
                        timeout=self.lock_timeout_sec,
                    ):
                        result = await self.refresh_service.refresh_due_accounts(
                            trigger="scheduler"
                        )
                        logger.info(
                            "Account scheduler iteration completed: checked={} refreshed={} recovered={} expired={} disabled={} rate_limited={} failed={}",
                            result.checked,
                            result.refreshed,
                            result.recovered,
                            result.expired,
                            result.disabled,
                            result.rate_limited,
                            result.failed,
                        )
                except StorageError:
                    logger.info("Account scheduler skipped: lock not acquired")
                await asyncio.sleep(self.interval_seconds)
            except asyncio.CancelledError:
                break
            except Exception as error:
                logger.error("Account scheduler error: {}", error)
                await asyncio.sleep(self.interval_seconds)

    def start(self) -> None:
        if self._running:
            logger.warning("Account scheduler already running")
            return
        self._running = True
        self._task = asyncio.create_task(self._refresh_loop())
        logger.info("Account scheduler enabled")

    def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        if self._task:
            self._task.cancel()
        logger.info("Account scheduler stopped")


_scheduler: Optional[AccountRefreshScheduler] = None


def get_account_refresh_scheduler(
    refresh_service: AccountRefreshService,
    *,
    interval_seconds: Optional[int] = None,
) -> AccountRefreshScheduler:
    global _scheduler
    if _scheduler is None:
        _scheduler = AccountRefreshScheduler(
            refresh_service,
            interval_seconds=interval_seconds,
        )
    return _scheduler


__all__ = [
    "AccountRefreshPolicy",
    "AccountRefreshScheduler",
    "get_account_refresh_scheduler",
]
