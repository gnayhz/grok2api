"""Account refresh service — mode-aware usage synchronisation."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from app.platform.config.snapshot import get_config
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms
from app.platform.runtime.batch import run_batch
from app.control.model.enums import ALL_MODES, ModeId
from .enums import AccountStatus, FeedbackKind, QuotaSource
from .models import AccountRecord, QuotaWindow
from .quota_defaults import default_quota_set
from .state_machine import AccountFeedback, apply_feedback

if TYPE_CHECKING:
    from .repository import AccountRepository


@dataclass
class RefreshResult:
    checked:      int = 0
    refreshed:    int = 0
    recovered:    int = 0
    expired:      int = 0
    disabled:     int = 0
    rate_limited: int = 0
    failed:       int = 0

    def merge(self, other: "RefreshResult") -> None:
        self.checked      += other.checked
        self.refreshed    += other.refreshed
        self.recovered    += other.recovered
        self.expired      += other.expired
        self.disabled     += other.disabled
        self.rate_limited += other.rate_limited
        self.failed       += other.failed


class AccountRefreshService:
    """Fetches real quota data from the upstream usage API and persists it.

    Triggers:
      1. Import   — super accounts: fetch all 3 modes.
      2. Call     — super accounts: fetch the called mode (async, non-blocking).
      3. Schedule — super: fetch all 3 modes; basic: static window reset check.
    """

    def __init__(self, repository: "AccountRepository") -> None:
        self._repo     = repository
        self._lock     = asyncio.Lock()
        self._od_lock  = asyncio.Lock()
        self._od_last  = 0.0

    # ------------------------------------------------------------------
    # Usage API fetch (delegates to dataplane reverse protocol)
    # ------------------------------------------------------------------

    async def _fetch_all_quotas(self, token: str) -> dict[int, QuotaWindow] | None:
        """Fetch quota windows for all three modes.  Returns {mode_id: window}."""
        try:
            from app.dataplane.reverse.protocol.xai_usage import fetch_all_quotas
            return await fetch_all_quotas(token)
        except Exception as exc:
            logger.debug("Usage fetch failed: token={}... error={}", token[:10], exc)
            return None

    async def _fetch_mode_quota(self, token: str, mode_id: int) -> QuotaWindow | None:
        """Fetch a single mode quota window."""
        try:
            from app.dataplane.reverse.protocol.xai_usage import fetch_mode_quota
            return await fetch_mode_quota(token, mode_id)
        except Exception as exc:
            logger.debug("Usage fetch failed: token={}... mode={} error={}", token[:10], mode_id, exc)
            return None

    # ------------------------------------------------------------------
    # Core refresh logic
    # ------------------------------------------------------------------

    async def refresh_on_import(self, tokens: list[str]) -> RefreshResult:
        """Called after bulk import — sync real quotas for all accounts."""
        records = await self._repo.get_accounts(tokens)
        active  = [r for r in records if not r.is_deleted()]
        if not active:
            return RefreshResult(checked=len(records))

        concurrency = get_config("account.refresh.usage_concurrency", 10)
        results = await run_batch(active, self._refresh_one, concurrency=concurrency)
        agg = RefreshResult(checked=len(records))
        for r in results:
            agg.merge(r)
        return agg

    async def refresh_call_async(self, token: str, mode_id: int) -> None:
        """Fire-and-forget quota sync after a successful call (all accounts)."""
        record = (await self._repo.get_accounts([token]) or [None])[0]
        if record is None or record.is_deleted():
            return
        window = await self._fetch_mode_quota(token, mode_id)
        await self._apply_single_mode(record, mode_id, window)

    async def refresh_scheduled(self) -> RefreshResult:
        """Periodic refresh — super: fetch API; basic: static reset check."""
        snapshot = await self._repo.runtime_snapshot()
        records  = snapshot.items

        concurrency = get_config("account.refresh.usage_concurrency", 10)
        results = await run_batch(
            records,
            self._refresh_one,
            concurrency=concurrency,
        )
        agg = RefreshResult()
        for r in results:
            agg.merge(r)
        return agg

    async def refresh_on_demand(self) -> RefreshResult:
        """Throttled on-demand refresh triggered by request path."""
        min_interval = float(get_config("account.refresh.on_demand_min_interval_sec", 300))
        import time
        now = time.monotonic()
        if now - self._od_last < min_interval:
            return RefreshResult()
        if self._od_lock.locked():
            return RefreshResult()
        async with self._od_lock:
            now = time.monotonic()
            if now - self._od_last < min_interval:
                return RefreshResult()
            result = await self.refresh_scheduled()
            self._od_last = time.monotonic()
            return result

    async def refresh_tokens(self, tokens: list[str]) -> RefreshResult:
        """Explicit refresh for a list of tokens (admin / manual trigger)."""
        records = await self._repo.get_accounts(tokens)
        concurrency = get_config("account.refresh.usage_concurrency", 10)
        results = await run_batch(records, self._refresh_one, concurrency=concurrency)
        agg = RefreshResult()
        for r in results:
            agg.merge(r)
        return agg

    # ------------------------------------------------------------------
    # Per-account refresh
    # ------------------------------------------------------------------

    async def _refresh_one(self, record: AccountRecord) -> RefreshResult:
        """Fetch all 3 modes for any account type; apply real data or fall back gracefully."""
        if record.is_deleted():
            return RefreshResult()

        windows   = await self._fetch_all_quotas(record.token)
        qs        = record.quota_set()
        now       = now_ms()
        patches:  dict[str, dict] = {}
        refreshed = False

        _MODE_KEYS = {0: "quota_auto", 1: "quota_fast", 2: "quota_expert"}

        for mode in ALL_MODES:
            mode_id = int(mode)

            if windows and mode_id in windows:
                # ✅ Got real data from API.
                patches[_MODE_KEYS[mode_id]] = windows[mode_id].to_dict()
                refreshed = True
                continue

            # ❌ API failed for this mode — apply fallback strategy.
            existing = qs.get(mode_id)

            if existing.source == QuotaSource.REAL:
                # Had real data before: decrement by 1 (conservative estimate).
                patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                    remaining      = max(0, existing.remaining - 1),
                    total          = existing.total,
                    window_seconds = existing.window_seconds,
                    reset_at       = existing.reset_at,
                    synced_at      = existing.synced_at,
                    source         = QuotaSource.ESTIMATED,
                ).to_dict()
            elif existing.is_window_expired(now):
                # Default/estimated data and window has expired: reset to defaults.
                default = default_quota_set(record.pool).get(mode_id)
                patches[_MODE_KEYS[mode_id]] = QuotaWindow(
                    remaining      = default.total,
                    total          = default.total,
                    window_seconds = default.window_seconds,
                    reset_at       = now + default.window_seconds * 1000,
                    synced_at      = now,
                    source         = QuotaSource.DEFAULT,
                ).to_dict()
            # else: window still valid, leave unchanged.

        if not patches:
            return RefreshResult(checked=1)

        from .commands import AccountPatch
        await self._repo.patch_accounts([
            AccountPatch(
                token            = record.token,
                last_sync_at     = now_ms() if refreshed else None,
                usage_sync_delta = 1 if refreshed else None,
                **patches,  # type: ignore[arg-type]
            )
        ])
        was_cooling = record.status == AccountStatus.COOLING
        return RefreshResult(
            checked=1,
            refreshed=1 if refreshed else 0,
            recovered=1 if (was_cooling and refreshed) else 0,
        )

    async def _apply_single_mode(
        self,
        record: AccountRecord,
        mode_id: int,
        window: QuotaWindow | None,
    ) -> None:
        qs = record.quota_set()
        if window is not None:
            win = window
        else:
            # API failed after a real call — always decrement by 1 since the call consumed quota.
            existing = qs.get(mode_id)
            win = QuotaWindow(
                remaining      = max(0, existing.remaining - 1),
                total          = existing.total,
                window_seconds = existing.window_seconds,
                reset_at       = existing.reset_at,
                synced_at      = existing.synced_at,
                source         = QuotaSource.ESTIMATED,
            )
        mode_key = {0: "quota_auto", 1: "quota_fast", 2: "quota_expert"}[mode_id]
        from .commands import AccountPatch
        await self._repo.patch_accounts([
            AccountPatch(
                token        = record.token,
                last_sync_at = now_ms() if window is not None else None,
                usage_sync_delta = 1 if window is not None else None,
                **{mode_key: win.to_dict()},  # type: ignore[arg-type]
            )
        ])


__all__ = ["AccountRefreshService", "RefreshResult"]
