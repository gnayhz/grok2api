"""
Cooling-account refresh and recovery services.
"""

from __future__ import annotations

import asyncio
import random
import time
from typing import TYPE_CHECKING, Optional

from pydantic import BaseModel, Field

from app.services.config import get_config
from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.core.storage import StorageError, get_storage
from app.services.account.models import AccountRecord
from app.services.account.service import RuntimeAccountService
from app.services.account.state_machine import (
    AccountFeedback,
    AccountFeedbackKind,
    AccountLifecycleState,
    derive_state,
    needs_refresh,
    now_ms,
    refresh_due_at,
)
from app.services.reverse.utils.retry import extract_retry_after

if TYPE_CHECKING:
    from app.services.grok.batch_services.usage import UsageService

DEFAULT_REFRESH_BATCH_SIZE = 10
DEFAULT_REFRESH_CONCURRENCY = 5
DEFAULT_REFRESH_INTERVAL_HOURS = 8
DEFAULT_SUPER_REFRESH_INTERVAL_HOURS = 2
DEFAULT_ON_DEMAND_REFRESH_MIN_INTERVAL_SEC = 300
DEFAULT_ON_DEMAND_REFRESH_MAX_TOKENS = 100
DEFAULT_ON_DEMAND_REFRESH_LOCK_TIMEOUT_SEC = 5
DEFAULT_SCHEDULER_INTERVAL_SEC = 300
DEFAULT_REFRESH_BATCH_PAUSE_SEC = 1.0
SUPER_WINDOW_THRESHOLD_SECONDS = 14_400
SUPER_POOL_NAME = "ssoSuper"
BASIC_POOL_NAME = "ssoBasic"
DEFAULT_RETRY_CODES = {408, 429, 500, 502, 503, 504}
DEFAULT_RETRY_BUDGET_SEC = 30.0
DEFAULT_RETRY_BACKOFF_BASE_SEC = 0.5
DEFAULT_RETRY_BACKOFF_FACTOR = 2.0
DEFAULT_RETRY_BACKOFF_MAX_SEC = 8.0


class AccountRefreshPolicy(BaseModel):
    batch_size: int = Field(default=DEFAULT_REFRESH_BATCH_SIZE, ge=1)
    concurrency: int = Field(default=DEFAULT_REFRESH_CONCURRENCY, ge=1)
    refresh_interval_hours: int = Field(default=DEFAULT_REFRESH_INTERVAL_HOURS, ge=1)
    super_refresh_interval_hours: int = Field(
        default=DEFAULT_SUPER_REFRESH_INTERVAL_HOURS, ge=1
    )
    scheduler_interval_sec: int = Field(default=DEFAULT_SCHEDULER_INTERVAL_SEC, ge=1)
    batch_pause_sec: float = Field(default=DEFAULT_REFRESH_BATCH_PAUSE_SEC, ge=0.0)
    on_demand_enabled: bool = True
    on_demand_min_interval_sec: float = Field(
        default=DEFAULT_ON_DEMAND_REFRESH_MIN_INTERVAL_SEC,
        ge=0.0,
    )
    on_demand_max_tokens: int = Field(
        default=DEFAULT_ON_DEMAND_REFRESH_MAX_TOKENS,
        ge=1,
    )
    on_demand_lock_timeout_sec: int = Field(
        default=DEFAULT_ON_DEMAND_REFRESH_LOCK_TIMEOUT_SEC,
        ge=1,
    )

    @classmethod
    def from_config(cls) -> "AccountRefreshPolicy":
        def _get_int(key: str, default: int, minimum: int) -> int:
            value = get_config(key, default)
            try:
                value = int(value)
            except (TypeError, ValueError):
                value = default
            return max(minimum, value)

        def _get_float(key: str, default: float, minimum: float) -> float:
            value = get_config(key, default)
            try:
                value = float(value)
            except (TypeError, ValueError):
                value = default
            return max(minimum, value)

        def _get_bool(key: str, default: bool) -> bool:
            value = get_config(key, default)
            if isinstance(value, bool):
                return value
            if isinstance(value, str):
                return value.strip().lower() in {"1", "true", "yes", "on"}
            return bool(value)

        return cls(
            refresh_interval_hours=_get_int(
                "account.refresh.interval_hours",
                DEFAULT_REFRESH_INTERVAL_HOURS,
                1,
            ),
            super_refresh_interval_hours=_get_int(
                "account.refresh.super_interval_hours",
                DEFAULT_SUPER_REFRESH_INTERVAL_HOURS,
                1,
            ),
            on_demand_enabled=_get_bool(
                "account.refresh.on_demand_enabled",
                True,
            ),
            on_demand_min_interval_sec=_get_float(
                "account.refresh.on_demand_min_interval_sec",
                DEFAULT_ON_DEMAND_REFRESH_MIN_INTERVAL_SEC,
                0.0,
            ),
            on_demand_max_tokens=_get_int(
                "account.refresh.on_demand_max_tokens",
                DEFAULT_ON_DEMAND_REFRESH_MAX_TOKENS,
                1,
            ),
        )


class AccountRefreshResult(BaseModel):
    checked: int = 0
    refreshed: int = 0
    recovered: int = 0
    expired: int = 0
    disabled: int = 0
    rate_limited: int = 0
    failed: int = 0

    def merge(self, other: "AccountRefreshResult") -> None:
        self.checked += other.checked
        self.refreshed += other.refreshed
        self.recovered += other.recovered
        self.expired += other.expired
        self.disabled += other.disabled
        self.rate_limited += other.rate_limited
        self.failed += other.failed


class AccountRefreshService:
    def __init__(
        self,
        runtime_service: RuntimeAccountService,
        *,
        usage_service: Optional["UsageService"] = None,
        policy: Optional[AccountRefreshPolicy] = None,
    ):
        self.runtime_service = runtime_service
        if usage_service is None:
            from app.services.grok.batch_services.usage import UsageService

            usage_service = UsageService()
        self.usage_service = usage_service
        self.policy = policy or AccountRefreshPolicy.from_config()
        self._refresh_lock = asyncio.Lock()
        self._on_demand_refresh_lock = asyncio.Lock()
        self._last_on_demand_refresh_at = 0.0

    def _is_consumed_mode(self) -> bool:
        value = get_config("account.runtime.consumed_mode_enabled", False)
        if isinstance(value, bool):
            return value
        if isinstance(value, str):
            return value.strip().lower() in {"1", "true", "yes", "on"}
        return bool(value)

    def _interval_ms_for_pool(self, pool_name: str) -> int:
        hours = (
            self.policy.super_refresh_interval_hours
            if pool_name == SUPER_POOL_NAME
            else self.policy.refresh_interval_hours
        )
        return max(1, int(hours)) * 3600 * 1000

    def _extract_status(self, error: Exception) -> Optional[int]:
        if isinstance(error, UpstreamException):
            if error.details and "status" in error.details:
                return error.details["status"]
            return getattr(error, "status_code", None)
        return None

    def _retry_codes(self) -> set[int]:
        value = get_config("retry.retry_status_codes", sorted(DEFAULT_RETRY_CODES))
        if isinstance(value, str):
            value = [part.strip() for part in value.split(",")]
        if not isinstance(value, (list, tuple, set)):
            return set(DEFAULT_RETRY_CODES)
        codes: set[int] = set()
        for item in value:
            try:
                codes.add(int(item))
            except (TypeError, ValueError):
                continue
        return codes or set(DEFAULT_RETRY_CODES)

    def _retry_budget(self) -> float:
        value = get_config("retry.retry_budget", DEFAULT_RETRY_BUDGET_SEC)
        try:
            return max(0.0, float(value))
        except (TypeError, ValueError):
            return DEFAULT_RETRY_BUDGET_SEC

    def _backoff_param(self, key: str, default: float, minimum: float) -> float:
        value = get_config(key, default)
        try:
            value = float(value)
        except (TypeError, ValueError):
            value = default
        return max(minimum, value)

    def _should_retry(self, status: int, error: Exception, attempt: int, budget_used: float) -> bool:
        if attempt >= 2:
            return False
        if budget_used >= self._retry_budget():
            return False
        if status not in self._retry_codes():
            return False
        if isinstance(error, UpstreamException) and error.details:
            if error.details.get("is_token_expired", False):
                return False
        return True

    def _calculate_delay(
        self,
        *,
        status: int,
        attempt: int,
        last_delay: float,
        retry_after: Optional[float],
    ) -> float:
        backoff_base = self._backoff_param(
            "retry.retry_backoff_base",
            DEFAULT_RETRY_BACKOFF_BASE_SEC,
            0.01,
        )
        backoff_factor = self._backoff_param(
            "retry.retry_backoff_factor",
            DEFAULT_RETRY_BACKOFF_FACTOR,
            1.0,
        )
        backoff_max = self._backoff_param(
            "retry.retry_backoff_max",
            DEFAULT_RETRY_BACKOFF_MAX_SEC,
            backoff_base,
        )
        if retry_after is not None and retry_after > 0:
            return min(retry_after, backoff_max)
        if status == 429:
            return min(random.uniform(backoff_base, max(backoff_base, last_delay * 3)), backoff_max)
        exp_delay = backoff_base * (backoff_factor**attempt)
        return random.uniform(0, min(exp_delay, backoff_max))

    def _extract_window_size_seconds(self, result: dict) -> Optional[int]:
        if not isinstance(result, dict):
            return None
        for key in ("windowSizeSeconds", "window_size_seconds"):
            if key in result:
                try:
                    return int(result.get(key))
                except (TypeError, ValueError):
                    return None
        limits = result.get("limits") or result.get("rateLimits")
        if isinstance(limits, dict):
            for key in ("windowSizeSeconds", "window_size_seconds"):
                if key in limits:
                    try:
                        return int(limits.get(key))
                    except (TypeError, ValueError):
                        return None
        return None

    def _target_pool_name(self, current_pool: str, result: dict) -> str:
        window_size = self._extract_window_size_seconds(result)
        if window_size is None:
            return current_pool
        if current_pool == SUPER_POOL_NAME and window_size >= SUPER_WINDOW_THRESHOLD_SECONDS:
            return BASIC_POOL_NAME
        if current_pool == BASIC_POOL_NAME and window_size < SUPER_WINDOW_THRESHOLD_SECONDS:
            return SUPER_POOL_NAME
        return current_pool

    async def _get_usage_with_retry(
        self,
        token: str,
    ) -> tuple[Optional[dict], Optional[int], Optional[Exception]]:
        attempt = 0
        total_delay = 0.0
        last_delay = self._backoff_param(
            "retry.retry_backoff_base",
            DEFAULT_RETRY_BACKOFF_BASE_SEC,
            0.01,
        )
        while True:
            try:
                return await self.usage_service.get(token), None, None
            except Exception as error:
                status = self._extract_status(error)
                if status is None:
                    return None, None, error
                if not self._should_retry(status, error, attempt, total_delay):
                    return None, status, error
                retry_after = extract_retry_after(error)
                delay = self._calculate_delay(
                    status=status,
                    attempt=attempt + 1,
                    last_delay=last_delay,
                    retry_after=retry_after,
                )
                if total_delay + delay > self._retry_budget():
                    return None, status, error
                attempt += 1
                total_delay += delay
                last_delay = delay
                logger.warning(
                    "Account refresh retry token={} attempt={}/{} status={} delay={:.2f}s",
                    token[:10] + "...",
                    attempt,
                    2,
                    status,
                    delay,
                )
                await asyncio.sleep(delay)

    async def collect_due_accounts(
        self,
        *,
        max_tokens: Optional[int] = None,
    ) -> list[AccountRecord]:
        await self.runtime_service.refresh_if_changed()
        cooling = self.runtime_service.list_runtime_accounts(
            states={AccountLifecycleState.COOLING}
        )
        now = now_ms()
        due_records: list[tuple[int, AccountRecord]] = []
        for record in cooling:
            interval_ms = self._interval_ms_for_pool(record.pool_name)
            if not needs_refresh(record, now=now, interval_ms=interval_ms):
                continue
            due_at = refresh_due_at(record, interval_ms=interval_ms) or 0
            due_records.append((due_at, record))

        due_records.sort(
            key=lambda item: (
                item[0],
                item[1].last_sync_at or 0,
                item[1].last_used_at or 0,
                item[1].created_at or 0,
            )
        )
        selected = [record for _, record in due_records]
        if max_tokens is not None and max_tokens > 0:
            selected = selected[:max_tokens]
        return selected

    async def collect_accounts(
        self,
        tokens: list[str],
    ) -> list[AccountRecord]:
        await self.runtime_service.refresh_if_changed()
        records: list[AccountRecord] = []
        for token in tokens:
            record = self.runtime_service.get_account(token)
            if record is not None:
                records.append(record)
        return records

    async def _refresh_one(self, record: AccountRecord) -> AccountRefreshResult:
        token = record.token
        result, status, error = await self._get_usage_with_retry(token)
        if result:
            new_quota = result.get("remainingTokens")
            if new_quota is None:
                new_quota = result.get("remainingQueries")
            if new_quota is None:
                return AccountRefreshResult(checked=1, failed=1)

            updated = await self.runtime_service.apply_feedback(
                AccountFeedback(
                    kind=AccountFeedbackKind.SUCCESS,
                    consumed_mode=self._is_consumed_mode(),
                    quota_remaining=int(new_quota),
                    apply_usage=False,
                ),
                token=token,
                persist=False,
            )
            if updated is None:
                return AccountRefreshResult(checked=1, failed=1)

            updated.last_sync_at = now_ms()
            updated.pool_name = self._target_pool_name(updated.pool_name, result)
            await self.runtime_service.upsert_runtime_record(updated, persist=True)
            return AccountRefreshResult(
                checked=1,
                refreshed=1,
                recovered=1 if int(new_quota) > 0 and record.quota == 0 else 0,
            )

        if status == 401:
            confirmed_expired = (
                isinstance(error, UpstreamException)
                and isinstance(error.details, dict)
                and error.details.get("is_token_expired", False)
            )
            if confirmed_expired:
                await self.runtime_service.apply_feedback(
                    AccountFeedback(
                        kind=AccountFeedbackKind.UNAUTHORIZED,
                        status_code=401,
                        reason="refresh_auth_failed",
                        confirm_expired=True,
                        apply_usage=False,
                    ),
                    token=token,
                    persist=True,
                )
                return AccountRefreshResult(checked=1, expired=1)
            logger.warning(
                "Account refresh received unconfirmed 401 for token={}",
                token[:10] + "...",
            )
            return AccountRefreshResult(checked=1, failed=1)

        if status == 403:
            updated = await self.runtime_service.apply_feedback(
                AccountFeedback(
                    kind=AccountFeedbackKind.FORBIDDEN,
                    status_code=403,
                    reason="refresh_forbidden",
                    apply_usage=False,
                ),
                token=token,
                persist=True,
            )
            disabled = int(
                updated is not None
                and derive_state(updated) == AccountLifecycleState.DISABLED
            )
            return AccountRefreshResult(checked=1, disabled=disabled, failed=1 - disabled)

        if status == 429:
            retry_after = extract_retry_after(error)
            await self.runtime_service.apply_feedback(
                AccountFeedback(
                    kind=AccountFeedbackKind.RATE_LIMITED,
                    status_code=429,
                    reason="refresh_rate_limited",
                    retry_after_ms=int(retry_after * 1000) if retry_after else None,
                    apply_usage=False,
                ),
                token=token,
                persist=True,
            )
            return AccountRefreshResult(checked=1, rate_limited=1)

        if error is not None:
            logger.warning(
                "Account refresh failed token={} error={}",
                token[:10] + "...",
                error,
            )
        return AccountRefreshResult(checked=1, failed=1)

    async def refresh_due_accounts(
        self,
        *,
        trigger: str = "scheduler",
        max_tokens: Optional[int] = None,
    ) -> AccountRefreshResult:
        async with self._refresh_lock:
            candidates = await self.collect_due_accounts(max_tokens=max_tokens)
            if not candidates:
                logger.debug("Account refresh skipped: trigger={} no due accounts", trigger)
                return AccountRefreshResult()

            logger.info(
                "Account refresh starting: trigger={} candidates={} limit={}",
                trigger,
                len(candidates),
                max_tokens or len(candidates),
            )

            semaphore = asyncio.Semaphore(self.policy.concurrency)

            async def _guarded_refresh(item: AccountRecord) -> AccountRefreshResult:
                async with semaphore:
                    return await self._refresh_one(item)

            aggregate = AccountRefreshResult()
            for index in range(0, len(candidates), self.policy.batch_size):
                batch = candidates[index : index + self.policy.batch_size]
                results = await asyncio.gather(*[_guarded_refresh(item) for item in batch])
                for item in results:
                    aggregate.merge(item)
                if (
                    index + self.policy.batch_size < len(candidates)
                    and self.policy.batch_pause_sec > 0
                ):
                    await asyncio.sleep(self.policy.batch_pause_sec)

            logger.info(
                "Account refresh completed: trigger={} checked={} refreshed={} recovered={} expired={} disabled={} rate_limited={} failed={}",
                trigger,
                aggregate.checked,
                aggregate.refreshed,
                aggregate.recovered,
                aggregate.expired,
                aggregate.disabled,
                aggregate.rate_limited,
                aggregate.failed,
            )
            return aggregate

    async def refresh_accounts(
        self,
        tokens: list[str],
        *,
        trigger: str = "manual",
    ) -> AccountRefreshResult:
        normalized = list(dict.fromkeys(token.removeprefix("sso=") for token in tokens if token))
        if not normalized:
            return AccountRefreshResult()
        async with self._refresh_lock:
            candidates = await self.collect_accounts(normalized)
            if not candidates:
                logger.debug("Account refresh skipped: trigger={} no matching accounts", trigger)
                return AccountRefreshResult()

            semaphore = asyncio.Semaphore(self.policy.concurrency)

            async def _guarded_refresh(item: AccountRecord) -> AccountRefreshResult:
                async with semaphore:
                    return await self._refresh_one(item)

            aggregate = AccountRefreshResult()
            results = await asyncio.gather(*[_guarded_refresh(item) for item in candidates])
            for item in results:
                aggregate.merge(item)
            logger.info(
                "Account refresh completed: trigger={} checked={} refreshed={} recovered={} expired={} disabled={} rate_limited={} failed={}",
                trigger,
                aggregate.checked,
                aggregate.refreshed,
                aggregate.recovered,
                aggregate.expired,
                aggregate.disabled,
                aggregate.rate_limited,
                aggregate.failed,
            )
            return aggregate

    async def refresh_due_accounts_on_demand(self) -> AccountRefreshResult:
        if not self.policy.on_demand_enabled:
            logger.debug("Account on-demand refresh skipped: disabled")
            return AccountRefreshResult()

        if self._on_demand_refresh_lock.locked():
            logger.debug("Account on-demand refresh skipped: already running")
            return AccountRefreshResult()

        now = time.monotonic()
        if (
            self.policy.on_demand_min_interval_sec > 0
            and self._last_on_demand_refresh_at > 0
            and now - self._last_on_demand_refresh_at < self.policy.on_demand_min_interval_sec
        ):
            logger.debug("Account on-demand refresh skipped: interval not reached")
            return AccountRefreshResult()

        async with self._on_demand_refresh_lock:
            now = time.monotonic()
            if (
                self.policy.on_demand_min_interval_sec > 0
                and self._last_on_demand_refresh_at > 0
                and now - self._last_on_demand_refresh_at < self.policy.on_demand_min_interval_sec
            ):
                logger.debug("Account on-demand refresh skipped after lock: interval not reached")
                return AccountRefreshResult()

            storage = get_storage()
            try:
                async with storage.acquire_lock(
                    "account_on_demand_refresh",
                    timeout=self.policy.on_demand_lock_timeout_sec,
                ):
                    result = await self.refresh_due_accounts(
                        trigger="on_demand",
                        max_tokens=self.policy.on_demand_max_tokens,
                    )
                    self._last_on_demand_refresh_at = time.monotonic()
                    return result
            except StorageError as error:
                logger.debug("Account on-demand refresh skipped: {}", error)
                return AccountRefreshResult()
