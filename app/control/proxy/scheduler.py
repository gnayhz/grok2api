"""Proxy clearance refresh scheduler.

Periodically refreshes ClearanceBundles for managed (FlareSolverr) mode.
Previously inline in ProxyDirectory; extracted for separation of concerns.
"""

import asyncio

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.control.proxy import ProxyDirectory


class ProxyClearanceScheduler:
    """Periodically refreshes proxy clearance bundles."""

    def __init__(self, directory: ProxyDirectory) -> None:
        self._directory = directory
        self._task: asyncio.Task | None = None
        self._running = False

    def start(self) -> None:
        if self._running:
            return
        self._running = True
        self._task = asyncio.create_task(self._loop())
        logger.info("ProxyClearanceScheduler started")

    def stop(self) -> None:
        self._running = False
        if self._task and not self._task.done():
            self._task.cancel()
        logger.info("ProxyClearanceScheduler stopped")

    async def _loop(self) -> None:
        while self._running:
            try:
                interval = self._get_interval()
                await asyncio.sleep(interval)
                if not self._running:
                    break
                await self._refresh()
            except asyncio.CancelledError:
                break
            except Exception as exc:
                logger.error("ProxyClearanceScheduler error: {}", exc)
                await asyncio.sleep(60)

    async def _refresh(self) -> None:
        """Reload proxy configuration (which triggers bundle refresh)."""
        try:
            await self._directory.load()
            logger.debug("Proxy clearance refresh completed")
        except Exception as exc:
            logger.warning("Proxy clearance refresh failed: {}", exc)

    def _get_interval(self) -> int:
        """Return refresh interval in seconds from config."""
        cfg = get_config()
        return cfg.get_int("proxy.clearance.refresh_interval", 600)


__all__ = ["ProxyClearanceScheduler"]
