"""
Unified service entry for the proxy / clearance domain.
"""

from __future__ import annotations

import asyncio

from app.core.logger import logger
from app.services.proxy.config import egress_affinity_key, load_proxy_domain_config
from app.services.proxy.models import (
    ClearanceMode,
    ProxyFeedback,
    ProxyLease,
    ProxyScope,
    RequestKind,
)
from app.services.proxy.providers.flaresolverr import FlareSolverrClearanceProvider
from app.services.proxy.runtime import ProxyRuntime


class ProxyService:
    def __init__(self):
        self.runtime = ProxyRuntime()
        self._managed_provider = FlareSolverrClearanceProvider()
        self._refresh_lock = asyncio.Lock()

    @staticmethod
    def _scope_proxy_urls(scope: ProxyScope) -> list[str]:
        config = load_proxy_domain_config()
        scope_config = config.asset if scope == ProxyScope.ASSET else config.app
        if not scope_config.urls and scope_config.egress_mode.value == "direct":
            return [""]
        return list(scope_config.urls)

    async def refresh_managed_bundles(
        self,
        *,
        scope: ProxyScope | None = None,
    ) -> int:
        config = load_proxy_domain_config()
        if config.clearance.mode != ClearanceMode.MANAGED:
            return 0

        scopes = [scope] if scope is not None else [ProxyScope.APP, ProxyScope.ASSET]
        refreshed = 0
        async with self._refresh_lock:
            for target_scope in scopes:
                scope_config = config.asset if target_scope == ProxyScope.ASSET else config.app
                proxy_urls = scope_config.urls or [""]
                for proxy_url in proxy_urls:
                    bundle = await self._managed_provider.refresh_bundle(
                        config=config.clearance,
                        affinity_key=egress_affinity_key(target_scope, proxy_url),
                        proxy_url=proxy_url,
                    )
                    if bundle is None:
                        continue
                    await self.runtime.upsert_bundle(bundle)
                    refreshed += 1
        return refreshed

    async def acquire(
        self,
        *,
        scope: ProxyScope,
        request_kind: RequestKind = RequestKind.HTTP,
    ) -> ProxyLease | None:
        config = load_proxy_domain_config()
        lease = await self.runtime.acquire(
            config=config,
            scope=scope,
            request_kind=request_kind,
        )
        if lease is not None:
            return lease

        if config.clearance.mode != ClearanceMode.MANAGED:
            return None

        await self.refresh_managed_bundles(scope=scope)
        lease = await self.runtime.acquire(
            config=config,
            scope=scope,
            request_kind=request_kind,
        )
        if lease is None:
            logger.warning(
                "ProxyService acquire failed after managed refresh: scope={} kind={}",
                scope.value,
                request_kind.value,
            )
        return lease

    async def release(self, lease_id: str) -> bool:
        return await self.runtime.release(lease_id)

    async def report(self, lease_id: str, feedback: ProxyFeedback) -> bool:
        return await self.runtime.report(lease_id, feedback)


_proxy_service: ProxyService | None = None


def get_proxy_service() -> ProxyService:
    global _proxy_service
    if _proxy_service is None:
        _proxy_service = ProxyService()
    return _proxy_service


__all__ = ["ProxyService", "get_proxy_service"]
