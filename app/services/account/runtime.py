"""
High-concurrency runtime selector for the account domain.
"""

from __future__ import annotations

import asyncio
import random
import uuid
from dataclasses import dataclass, field
from typing import Callable, Optional, Sequence

from pydantic import BaseModel, Field

from app.core.logger import logger
from app.services.account.models import AccountRecord, EffortType, now_ms
from app.services.account.repository import AccountRepository
from app.services.account.state_machine import (
    AccountFeedback,
    AccountFeedbackKind,
    AccountLifecycleState,
    AccountStatePolicy,
    derive_state,
    is_selectable,
)


@dataclass(slots=True)
class AccountRuntimeEntry:
    record: AccountRecord
    inflight: int = 0
    health_score: float = 1.0
    last_reserved_at: Optional[int] = None
    last_feedback_at: Optional[int] = None
    consecutive_failures: int = 0
    successful_uses: int = 0


@dataclass(slots=True)
class AccountPoolIndex:
    all_tokens: set[str] = field(default_factory=set)
    active_tokens: set[str] = field(default_factory=set)
    quota_tokens: set[str] = field(default_factory=set)
    by_tag: dict[str, set[str]] = field(default_factory=dict)

    def add(self, record: AccountRecord) -> None:
        self.all_tokens.add(record.token)
        if derive_state(record) == AccountLifecycleState.ACTIVE:
            self.active_tokens.add(record.token)
            if record.quota > 0:
                self.quota_tokens.add(record.token)
        for tag in record.tags:
            self.by_tag.setdefault(tag, set()).add(record.token)

    def remove(self, record: AccountRecord) -> None:
        self.all_tokens.discard(record.token)
        self.active_tokens.discard(record.token)
        self.quota_tokens.discard(record.token)
        for tag in record.tags:
            tag_set = self.by_tag.get(tag)
            if not tag_set:
                continue
            tag_set.discard(record.token)
            if not tag_set:
                self.by_tag.pop(tag, None)


@dataclass(slots=True)
class AccountLease:
    lease_id: str
    token: str
    pool_name: str
    selected_at: int
    effort: EffortType
    consumed_mode: bool
    record: AccountRecord


class AccountSelectionPolicy(BaseModel):
    pool_priority_weight: float = Field(default=1000.0, ge=0.0)
    health_weight: float = Field(default=100.0, ge=0.0)
    quota_weight: float = Field(default=25.0, ge=0.0)
    consumed_weight: float = Field(default=25.0, ge=0.0)
    inflight_penalty: float = Field(default=20.0, ge=0.0)
    recent_use_penalty: float = Field(default=15.0, ge=0.0)
    fail_count_penalty: float = Field(default=4.0, ge=0.0)
    recent_use_window_ms: int = Field(default=15_000, ge=0)
    success_recovery_step: float = Field(default=0.12, ge=0.0)
    auth_failure_health_factor: float = Field(default=0.55, ge=0.0)
    forbidden_health_factor: float = Field(default=0.25, ge=0.0)
    rate_limited_health_factor: float = Field(default=0.45, ge=0.0)
    min_health_score: float = Field(default=0.05, ge=0.0)
    jitter: float = Field(default=0.05, ge=0.0)

    @classmethod
    def from_config(cls) -> "AccountSelectionPolicy":
        return cls()


class AccountDirectory:
    """
    Runtime memory directory for high-throughput account selection.

    Repository remains the source of truth for management and persistence.
    This directory keeps hot-path selection state such as health score and
    in-flight reservation counters.
    """

    def __init__(
        self,
        repository: AccountRepository,
        *,
        selection_policy: Optional[AccountSelectionPolicy] = None,
        state_policy: Optional[AccountStatePolicy] = None,
    ):
        self.repository = repository
        self.selection_policy = selection_policy or AccountSelectionPolicy.from_config()
        self.state_policy = state_policy or AccountStatePolicy.from_config()
        self.revision = 0
        self.entries: dict[str, AccountRuntimeEntry] = {}
        self.pools: dict[str, AccountPoolIndex] = {}
        self._leases: dict[str, str] = {}
        self._refresh_lock = asyncio.Lock()
        self._state_lock = asyncio.Lock()

    def _normalize_token(self, token: str) -> str:
        return token.removeprefix("sso=")

    def _remove_from_pool_indexes(self, record: AccountRecord) -> None:
        pool = self.pools.get(record.pool_name)
        if not pool:
            return
        pool.remove(record)
        if not pool.all_tokens:
            self.pools.pop(record.pool_name, None)

    def _add_to_pool_indexes(self, record: AccountRecord) -> None:
        pool = self.pools.setdefault(record.pool_name, AccountPoolIndex())
        pool.add(record)

    def _apply_upsert_locked(self, record: AccountRecord) -> None:
        previous_entry = self.entries.get(record.token)
        if previous_entry:
            self._remove_from_pool_indexes(previous_entry.record)
        if record.deleted_at is not None:
            self.entries.pop(record.token, None)
            return
        if previous_entry:
            previous_entry.record = record
            entry = previous_entry
        else:
            entry = AccountRuntimeEntry(record=record)
            self.entries[record.token] = entry
        self._add_to_pool_indexes(entry.record)

    def _apply_delete_locked(self, token: str) -> None:
        token = self._normalize_token(token)
        entry = self.entries.pop(token, None)
        if not entry:
            return
        self._remove_from_pool_indexes(entry.record)

    async def bootstrap(self) -> None:
        async with self._refresh_lock:
            snapshot = await self.repository.runtime_snapshot(include_deleted=False)
            async with self._state_lock:
                self.entries = {}
                self.pools = {}
                self._leases = {}
                for record in snapshot.items:
                    self._apply_upsert_locked(record)
                self.revision = snapshot.revision
            logger.info(
                "AccountDirectory bootstrapped: revision={} records={} pools={}",
                self.revision,
                len(self.entries),
                len(self.pools),
            )

    async def refresh_if_changed(self) -> bool:
        latest = await self.repository.get_revision()
        if latest <= self.revision:
            return False
        async with self._refresh_lock:
            latest = await self.repository.get_revision()
            if latest <= self.revision:
                return False
            while self.revision < latest:
                changes = await self.repository.scan_changes(self.revision, limit=5000)
                if (
                    not changes.items
                    and not changes.deleted_tokens
                    and changes.revision <= self.revision
                ):
                    break
                async with self._state_lock:
                    for token in changes.deleted_tokens:
                        self._apply_delete_locked(token)
                    for record in changes.items:
                        self._apply_upsert_locked(record)
                    self.revision = max(self.revision, changes.revision)
                if not changes.has_more:
                    break
            return True

    def get_record(self, token: str) -> Optional[AccountRecord]:
        entry = self.entries.get(self._normalize_token(token))
        if not entry:
            return None
        return entry.record.model_copy(deep=True)

    def list_records(
        self,
        *,
        pool_names: Optional[set[str]] = None,
        states: Optional[set[AccountLifecycleState]] = None,
    ) -> list[AccountRecord]:
        records: list[AccountRecord] = []
        for entry in self.entries.values():
            record = entry.record
            if pool_names and record.pool_name not in pool_names:
                continue
            if states and derive_state(record) not in states:
                continue
            records.append(record.model_copy(deep=True))
        return records

    async def upsert_record(self, record: AccountRecord) -> AccountRecord:
        async with self._state_lock:
            self._apply_upsert_locked(record)
            return record.model_copy(deep=True)

    def _collect_candidates_locked(
        self,
        pool_names: Sequence[str],
        *,
        exclude: Optional[set[str]],
        prefer_tags: Optional[set[str]],
        consumed_mode: bool,
    ) -> list[AccountRuntimeEntry]:
        exclude_tokens = {self._normalize_token(token) for token in (exclude or set())}
        candidates: list[AccountRuntimeEntry] = []
        for pool_name in pool_names:
            pool = self.pools.get(pool_name)
            if not pool:
                continue
            token_source = pool.active_tokens if consumed_mode else pool.quota_tokens
            token_set = set(token_source)
            if prefer_tags:
                preferred_set: Optional[set[str]] = None
                for tag in prefer_tags:
                    tag_tokens = pool.by_tag.get(tag, set())
                    preferred_set = (
                        set(tag_tokens)
                        if preferred_set is None
                        else preferred_set & set(tag_tokens)
                    )
                if preferred_set:
                    token_set &= preferred_set
            token_set -= exclude_tokens
            for token in token_set:
                entry = self.entries.get(token)
                if not entry:
                    continue
                if not is_selectable(entry.record, consumed_mode=consumed_mode):
                    continue
                candidates.append(entry)
        if candidates or not prefer_tags:
            return candidates

        fallback: list[AccountRuntimeEntry] = []
        for pool_name in pool_names:
            pool = self.pools.get(pool_name)
            if not pool:
                continue
            token_source = pool.active_tokens if consumed_mode else pool.quota_tokens
            for token in set(token_source) - exclude_tokens:
                entry = self.entries.get(token)
                if entry and is_selectable(entry.record, consumed_mode=consumed_mode):
                    fallback.append(entry)
        return fallback

    def _score_entry(
        self,
        entry: AccountRuntimeEntry,
        *,
        now: int,
        consumed_mode: bool,
        pool_priority: int,
        max_quota: int,
        min_consumed: int,
        max_consumed: int,
    ) -> float:
        policy = self.selection_policy
        score = float(pool_priority) * policy.pool_priority_weight
        score += entry.health_score * policy.health_weight
        if consumed_mode:
            spread = max(1, max_consumed - min_consumed)
            normalized = 1.0 - ((entry.record.consumed - min_consumed) / spread)
            score += normalized * policy.consumed_weight
        else:
            normalized = 0.0 if max_quota <= 0 else entry.record.quota / max_quota
            score += normalized * policy.quota_weight
        score -= entry.inflight * policy.inflight_penalty
        score -= min(entry.record.fail_count, 10) * policy.fail_count_penalty

        recent_basis = entry.last_reserved_at or entry.record.last_used_at
        if recent_basis and policy.recent_use_window_ms > 0:
            age = max(0, now - recent_basis)
            ratio = 1.0 - min(age, policy.recent_use_window_ms) / policy.recent_use_window_ms
            score -= ratio * policy.recent_use_penalty

        if policy.jitter > 0:
            score += random.random() * policy.jitter
        return score

    async def reserve(
        self,
        pool_names: str | Sequence[str],
        *,
        exclude: Optional[set[str]] = None,
        prefer_tags: Optional[set[str]] = None,
        consumed_mode: bool = False,
        effort: EffortType = EffortType.LOW,
    ) -> Optional[AccountLease]:
        requested_pools = (
            [pool_names] if isinstance(pool_names, str) else [pool for pool in pool_names if pool]
        )
        if not requested_pools:
            return None

        async with self._state_lock:
            now = now_ms()
            candidates = self._collect_candidates_locked(
                requested_pools,
                exclude=exclude,
                prefer_tags=prefer_tags,
                consumed_mode=consumed_mode,
            )
            if not candidates:
                return None

            pool_priority_map = {
                pool_name: len(requested_pools) - index
                for index, pool_name in enumerate(requested_pools)
            }
            max_quota = max((entry.record.quota for entry in candidates), default=1)
            min_consumed = min((entry.record.consumed for entry in candidates), default=0)
            max_consumed = max((entry.record.consumed for entry in candidates), default=0)
            scored: list[tuple[float, AccountRuntimeEntry]] = []
            for entry in candidates:
                priority = pool_priority_map.get(entry.record.pool_name, 0)
                score = self._score_entry(
                    entry,
                    now=now,
                    consumed_mode=consumed_mode,
                    pool_priority=priority,
                    max_quota=max_quota,
                    min_consumed=min_consumed,
                    max_consumed=max_consumed,
                )
                scored.append((score, entry))

            max_score = max(score for score, _ in scored)
            finalists = [
                entry for score, entry in scored if abs(score - max_score) < 1e-9
            ]
            selected = random.choice(finalists)
            selected.inflight += 1
            selected.last_reserved_at = now
            lease_id = uuid.uuid4().hex
            self._leases[lease_id] = selected.record.token
            return AccountLease(
                lease_id=lease_id,
                token=selected.record.token,
                pool_name=selected.record.pool_name,
                selected_at=now,
                effort=effort,
                consumed_mode=consumed_mode,
                record=selected.record.model_copy(deep=True),
            )

    async def release(self, lease_id: str) -> bool:
        async with self._state_lock:
            token = self._leases.pop(lease_id, None)
            if not token:
                return False
            entry = self.entries.get(token)
            if not entry:
                return False
            entry.inflight = max(0, entry.inflight - 1)
            return True

    def _update_health_locked(
        self,
        entry: AccountRuntimeEntry,
        feedback: AccountFeedback,
    ) -> None:
        policy = self.selection_policy
        if feedback.kind == AccountFeedbackKind.SUCCESS:
            entry.health_score = min(
                1.0, entry.health_score + policy.success_recovery_step
            )
            entry.consecutive_failures = 0
            entry.successful_uses += 1
            return
        if feedback.kind == AccountFeedbackKind.UNAUTHORIZED:
            factor = policy.auth_failure_health_factor
        elif feedback.kind == AccountFeedbackKind.FORBIDDEN:
            factor = policy.forbidden_health_factor
        elif feedback.kind == AccountFeedbackKind.RATE_LIMITED:
            factor = policy.rate_limited_health_factor
        else:
            factor = 1.0
        entry.health_score = max(
            policy.min_health_score,
            entry.health_score * factor,
        )
        entry.consecutive_failures += 1

    async def apply_feedback(
        self,
        feedback: AccountFeedback,
        *,
        token: Optional[str] = None,
        lease_id: Optional[str] = None,
        apply_state: Callable[..., AccountRecord],
    ) -> Optional[AccountRecord]:
        async with self._state_lock:
            lease_token = self._leases.pop(lease_id, None) if lease_id else None
            resolved_token = self._normalize_token(token or lease_token or "")
            if not resolved_token:
                return None
            entry = self.entries.get(resolved_token)
            if not entry:
                return None
            if lease_id:
                entry.inflight = max(0, entry.inflight - 1)
            previous = entry.record
            updated = apply_state(previous, feedback=feedback, policy=self.state_policy)
            entry.record = updated
            entry.last_feedback_at = feedback.at
            self._update_health_locked(entry, feedback)
            self._remove_from_pool_indexes(previous)
            if updated.deleted_at is not None:
                self.entries.pop(resolved_token, None)
            else:
                self._add_to_pool_indexes(updated)
            return updated.model_copy(deep=True)

    async def select(
        self,
        pool_names: str | Sequence[str],
        *,
        exclude: Optional[set[str]] = None,
        prefer_tags: Optional[set[str]] = None,
        consumed_mode: bool = False,
        effort: EffortType = EffortType.LOW,
    ) -> Optional[AccountRecord]:
        lease = await self.reserve(
            pool_names,
            exclude=exclude,
            prefer_tags=prefer_tags,
            consumed_mode=consumed_mode,
            effort=effort,
        )
        if not lease:
            return None
        await self.release(lease.lease_id)
        return lease.record
