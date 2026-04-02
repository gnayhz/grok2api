"""
Runtime state for the unified proxy / clearance domain.
"""

from __future__ import annotations

import asyncio
import time
import uuid
from dataclasses import dataclass

from app.core.logger import logger
from app.services.proxy.config import (
    ProxyDomainConfig,
    egress_affinity_key,
    get_scope_config,
)
from app.services.proxy.models import (
    ClearanceBundle,
    ClearanceBundleState,
    ClearanceMode,
    EgressMode,
    EgressNode,
    EgressNodeState,
    ProxyFeedback,
    ProxyFeedbackKind,
    ProxyLease,
    ProxyScope,
    RequestKind,
)
from app.services.proxy.providers.manual import ManualClearanceProvider


def _now_ms() -> int:
    return int(time.time() * 1000)


@dataclass(slots=True)
class EgressRuntimeEntry:
    node: EgressNode
    inflight: int = 0
    last_used_at: int | None = None
    last_error_at: int | None = None
    consecutive_transport_failures: int = 0
    cooldown_until: int | None = None


@dataclass(slots=True)
class BundleRuntimeEntry:
    bundle: ClearanceBundle
    last_used_at: int | None = None
    last_challenge_at: int | None = None
    refresh_failures: int = 0
    cooldown_until: int | None = None


class ProxyRuntime:
    def __init__(self):
        self._config_key: tuple | None = None
        self._nodes: dict[str, EgressRuntimeEntry] = {}
        self._bundles: dict[str, BundleRuntimeEntry] = {}
        self._leases: dict[str, ProxyLease] = {}
        self._preferred_node_ids: dict[ProxyScope, str] = {}
        self._lock = asyncio.Lock()
        self._manual_provider = ManualClearanceProvider()

    def _scope_nodes(self, scope: ProxyScope) -> list[EgressRuntimeEntry]:
        return [entry for entry in self._nodes.values() if entry.node.scope == scope]

    def _build_nodes_for_scope(self, config: ProxyDomainConfig, scope: ProxyScope) -> list[EgressNode]:
        scope_config = get_scope_config(config, scope)
        if scope_config.egress_mode == EgressMode.DIRECT:
            return [
                EgressNode(
                    node_id=f"{scope.value}:direct",
                    scope=scope,
                    mode=EgressMode.DIRECT,
                    proxy_url="",
                )
            ]
        return [
            EgressNode(
                node_id=f"{scope.value}:{index}",
                scope=scope,
                mode=scope_config.egress_mode,
                proxy_url=proxy_url,
            )
            for index, proxy_url in enumerate(scope_config.urls)
        ]

    def _merge_nodes(self, config: ProxyDomainConfig) -> None:
        next_nodes: dict[str, EgressRuntimeEntry] = {}
        for scope in (ProxyScope.APP, ProxyScope.ASSET):
            for node in self._build_nodes_for_scope(config, scope):
                existing = self._nodes.get(node.node_id)
                if existing:
                    existing.node = node
                    next_nodes[node.node_id] = existing
                else:
                    next_nodes[node.node_id] = EgressRuntimeEntry(node=node)
        self._nodes = next_nodes
        self._preferred_node_ids = {
            scope: node_id
            for scope, node_id in self._preferred_node_ids.items()
            if node_id in self._nodes
        }

    def _sync_manual_bundles(self, config: ProxyDomainConfig) -> None:
        if config.clearance.mode != ClearanceMode.MANUAL:
            self._bundles = {
                bundle_id: entry
                for bundle_id, entry in self._bundles.items()
                if entry.bundle.mode != ClearanceMode.MANUAL
            }
            return
        manual_bundles: dict[str, BundleRuntimeEntry] = {}
        for scope in (ProxyScope.APP, ProxyScope.ASSET):
            for entry in self._scope_nodes(scope):
                affinity_key = egress_affinity_key(
                    scope,
                    entry.node.proxy_url,
                )
                bundle = self._manual_provider.build_bundle(
                    config=config.clearance,
                    affinity_key=affinity_key,
                )
                if bundle is None:
                    continue
                existing = self._bundles.get(bundle.bundle_id)
                if existing:
                    existing.bundle = bundle
                    manual_bundles[bundle.bundle_id] = existing
                else:
                    manual_bundles[bundle.bundle_id] = BundleRuntimeEntry(bundle=bundle)
        for bundle_id, entry in self._bundles.items():
            if entry.bundle.mode != ClearanceMode.MANUAL:
                manual_bundles[bundle_id] = entry
        self._bundles = manual_bundles

    async def ensure_config(self, config: ProxyDomainConfig) -> bool:
        cache_key = config.cache_key()
        async with self._lock:
            if cache_key == self._config_key:
                return False
            self._merge_nodes(config)
            self._sync_manual_bundles(config)
            self._config_key = cache_key
            logger.info(
                "ProxyRuntime reloaded: nodes={} bundles={} clearance_mode={}",
                len(self._nodes),
                len(self._bundles),
                config.clearance.mode.value,
            )
            return True

    def _is_node_available(self, entry: EgressRuntimeEntry, now: int) -> bool:
        if entry.node.state == EgressNodeState.DISABLED:
            return False
        if entry.cooldown_until and entry.cooldown_until > now:
            return False
        return True

    def _is_bundle_available(self, entry: BundleRuntimeEntry, now: int) -> bool:
        if entry.bundle.state == ClearanceBundleState.EXPIRED:
            return False
        if entry.cooldown_until and entry.cooldown_until > now:
            return False
        return True

    def _select_node(self, scope: ProxyScope, now: int) -> EgressRuntimeEntry | None:
        candidates = [
            entry
            for entry in self._scope_nodes(scope)
            if self._is_node_available(entry, now)
        ]
        if not candidates:
            return None
        preferred_id = self._preferred_node_ids.get(scope)
        for entry in sorted(candidates, key=lambda item: item.node.node_id):
            if entry.node.node_id == preferred_id:
                return entry
        selected = min(
            candidates,
            key=lambda item: (
                item.inflight,
                item.node.node_id,
            ),
        )
        self._preferred_node_ids[scope] = selected.node.node_id
        return selected

    def _find_bundle(
        self,
        *,
        mode: ClearanceMode,
        affinity_key: str,
        now: int,
    ) -> BundleRuntimeEntry | None:
        if mode == ClearanceMode.NONE:
            return None
        bundle_id = f"{mode.value}:{affinity_key}"
        entry = self._bundles.get(bundle_id)
        if not entry:
            return None
        return entry if self._is_bundle_available(entry, now) else None

    async def acquire(
        self,
        *,
        config: ProxyDomainConfig,
        scope: ProxyScope,
        request_kind: RequestKind,
    ) -> ProxyLease | None:
        await self.ensure_config(config)
        async with self._lock:
            now = _now_ms()
            node_entry = self._select_node(scope, now)
            if node_entry is None:
                return None

            affinity_key = egress_affinity_key(
                scope,
                node_entry.node.proxy_url,
            )
            bundle_entry = self._find_bundle(
                mode=config.clearance.mode,
                affinity_key=affinity_key,
                now=now,
            )

            node_entry.inflight += 1
            node_entry.last_used_at = now
            if bundle_entry:
                bundle_entry.last_used_at = now

            lease = ProxyLease(
                lease_id=uuid.uuid4().hex,
                scope=scope,
                request_kind=request_kind,
                node_id=node_entry.node.node_id,
                bundle_id=bundle_entry.bundle.bundle_id if bundle_entry else None,
                proxy_url=node_entry.node.proxy_url,
                cf_cookies=bundle_entry.bundle.cf_cookies if bundle_entry else "",
                cf_clearance=bundle_entry.bundle.cf_clearance if bundle_entry else "",
                user_agent=bundle_entry.bundle.user_agent if bundle_entry else config.clearance.user_agent,
                browser=bundle_entry.bundle.browser if bundle_entry else config.clearance.browser,
                selected_at=now,
            )
            self._leases[lease.lease_id] = lease
            return lease

    async def upsert_bundle(self, bundle: ClearanceBundle) -> ClearanceBundle:
        async with self._lock:
            existing = self._bundles.get(bundle.bundle_id)
            if existing:
                existing.bundle = bundle
            else:
                self._bundles[bundle.bundle_id] = BundleRuntimeEntry(bundle=bundle)
            return bundle

    async def release(self, lease_id: str) -> bool:
        async with self._lock:
            lease = self._leases.pop(lease_id, None)
            if not lease:
                return False
            node_entry = self._nodes.get(lease.node_id)
            if node_entry:
                node_entry.inflight = max(0, node_entry.inflight - 1)
            return True

    async def report(self, lease_id: str, feedback: ProxyFeedback) -> bool:
        async with self._lock:
            lease = self._leases.pop(lease_id, None)
            if not lease:
                return False

            now = feedback.at
            node_entry = self._nodes.get(lease.node_id)
            bundle_entry = (
                self._bundles.get(lease.bundle_id) if lease.bundle_id else None
            )
            if node_entry:
                node_entry.inflight = max(0, node_entry.inflight - 1)

            if feedback.kind == ProxyFeedbackKind.SUCCESS:
                if node_entry:
                    node_entry.node.health_score = min(1.0, node_entry.node.health_score + 0.05)
                    node_entry.consecutive_transport_failures = 0
                    node_entry.cooldown_until = None
                    node_entry.node.state = EgressNodeState.ACTIVE
                    self._preferred_node_ids[lease.scope] = node_entry.node.node_id
                if bundle_entry:
                    bundle_entry.bundle.state = ClearanceBundleState.ACTIVE
                    bundle_entry.refresh_failures = 0
                    bundle_entry.cooldown_until = None
                return True

            if feedback.kind in (
                ProxyFeedbackKind.TRANSPORT_ERROR,
                ProxyFeedbackKind.UPSTREAM_5XX,
            ):
                if node_entry:
                    node_entry.node.health_score = max(0.1, node_entry.node.health_score * 0.6)
                    node_entry.last_error_at = now
                    node_entry.consecutive_transport_failures += 1
                    if node_entry.consecutive_transport_failures >= 2:
                        node_entry.cooldown_until = now + 30_000
                        node_entry.node.state = EgressNodeState.COOLING
                        self._preferred_node_ids.pop(lease.scope, None)
                return True

            if feedback.kind == ProxyFeedbackKind.RATE_LIMITED:
                if node_entry:
                    cooldown_ms = feedback.retry_after_ms or 30_000
                    node_entry.cooldown_until = now + cooldown_ms
                    node_entry.node.state = EgressNodeState.COOLING
                    self._preferred_node_ids.pop(lease.scope, None)
                return True

            if feedback.kind == ProxyFeedbackKind.CHALLENGE:
                if bundle_entry:
                    bundle_entry.bundle.state = ClearanceBundleState.COOLING
                    bundle_entry.cooldown_until = now + 60_000
                    bundle_entry.last_challenge_at = now
                    bundle_entry.refresh_failures += 1
                return True

            return True


__all__ = ["ProxyRuntime"]
