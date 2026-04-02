"""
Managed clearance scheduler for the proxy domain.
"""

from __future__ import annotations

import asyncio

from app.core.logger import logger
from app.services.proxy.config import load_proxy_domain_config
from app.services.proxy.models import ClearanceMode
from app.services.proxy.service import ProxyService, get_proxy_service


class ProxyRefreshScheduler:
    def __init__(self, service: ProxyService):
        self._service = service
        self._task: asyncio.Task | None = None

    async def _loop(self) -> None:
        logger.info("Proxy refresh scheduler started")
        while True:
            config = load_proxy_domain_config()
            if (
                config.clearance.mode == ClearanceMode.MANAGED
                and config.clearance.flaresolverr_url
            ):
                try:
                    refreshed = await self._service.refresh_managed_bundles()
                    logger.debug(
                        "Proxy refresh scheduler cycle finished: refreshed={}",
                        refreshed,
                    )
                except Exception as error:
                    logger.warning("Proxy refresh scheduler cycle failed: {}", error)
            await asyncio.sleep(config.clearance.refresh_interval_sec)

    def start(self) -> None:
        if self._task is not None:
            return
        self._task = asyncio.get_event_loop().create_task(self._loop())

    def stop(self) -> None:
        if self._task is None:
            return
        self._task.cancel()
        self._task = None
        logger.info("Proxy refresh scheduler stopped")


_proxy_refresh_scheduler: ProxyRefreshScheduler | None = None


def get_proxy_refresh_scheduler(
    service: ProxyService | None = None,
) -> ProxyRefreshScheduler:
    global _proxy_refresh_scheduler
    if _proxy_refresh_scheduler is None:
        _proxy_refresh_scheduler = ProxyRefreshScheduler(service or get_proxy_service())
    return _proxy_refresh_scheduler


__all__ = ["ProxyRefreshScheduler", "get_proxy_refresh_scheduler"]
