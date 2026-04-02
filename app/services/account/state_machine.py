"""
Lifecycle state machine for the account runtime domain.
"""

from __future__ import annotations

from enum import Enum
from typing import Optional

from pydantic import BaseModel, Field

from app.services.config import get_config
from app.services.account.models import AccountRecord, AccountStatus, EffortType, now_ms

EFFORT_COST: dict[EffortType, int] = {
    EffortType.LOW: 1,
    EffortType.HIGH: 4,
}

COOLDOWN_UNTIL_KEY = "cooldown_until"
COOLDOWN_REASON_KEY = "cooldown_reason"
COOLDOWN_STARTED_AT_KEY = "cooldown_started_at"
DISABLED_AT_KEY = "disabled_at"
DISABLED_REASON_KEY = "disabled_reason"
EXPIRED_AT_KEY = "expired_at"
EXPIRED_REASON_KEY = "expired_reason"
LAST_STATUS_CODE_KEY = "last_status_code"
LAST_RECOVERED_AT_KEY = "last_recovered_at"


class AccountLifecycleState(str, Enum):
    ACTIVE = "active"
    COOLING = "cooling"
    EXPIRED = "expired"
    DISABLED = "disabled"
    DELETED = "deleted"


class AccountFeedbackKind(str, Enum):
    SUCCESS = "success"
    UNAUTHORIZED = "unauthorized"
    FORBIDDEN = "forbidden"
    RATE_LIMITED = "rate_limited"
    DISABLE = "disable"
    DELETE = "delete"
    RESTORE = "restore"


class AccountStatePolicy(BaseModel):
    failure_threshold: int = Field(default=5, ge=1)
    forbidden_threshold: int = Field(default=1, ge=1)
    default_cooling_ms: int = Field(default=15 * 60 * 1000, ge=0)

    @classmethod
    def from_config(cls) -> "AccountStatePolicy":
        value = get_config("account.runtime.fail_threshold", 5)
        try:
            failure_threshold = max(1, int(value))
        except (TypeError, ValueError):
            failure_threshold = 5
        return cls(failure_threshold=failure_threshold)


class AccountFeedback(BaseModel):
    kind: AccountFeedbackKind
    at: int = Field(default_factory=now_ms)
    status_code: Optional[int] = None
    reason: str = ""
    effort: EffortType = EffortType.LOW
    consumed_mode: bool = False
    quota_remaining: Optional[int] = None
    retry_after_ms: Optional[int] = None
    confirm_expired: bool = False
    apply_usage: bool = True

    @classmethod
    def from_status_code(
        cls,
        status_code: int,
        *,
        reason: str = "",
        consumed_mode: bool = False,
        retry_after_ms: Optional[int] = None,
        confirm_expired: bool = False,
        effort: EffortType = EffortType.LOW,
        apply_usage: bool = False,
    ) -> "AccountFeedback":
        if status_code == 401:
            kind = AccountFeedbackKind.UNAUTHORIZED
        elif status_code == 403:
            kind = AccountFeedbackKind.FORBIDDEN
        elif status_code == 429:
            kind = AccountFeedbackKind.RATE_LIMITED
        else:
            kind = AccountFeedbackKind.SUCCESS
        return cls(
            kind=kind,
            status_code=status_code,
            reason=reason,
            consumed_mode=consumed_mode,
            retry_after_ms=retry_after_ms,
            confirm_expired=confirm_expired,
            effort=effort,
            apply_usage=apply_usage if kind != AccountFeedbackKind.SUCCESS else True,
        )


def effort_cost(effort: EffortType) -> int:
    return EFFORT_COST[effort]


def derive_state(record: AccountRecord) -> AccountLifecycleState:
    if record.deleted_at is not None:
        return AccountLifecycleState.DELETED
    return AccountLifecycleState(record.status.value)


def cooldown_until(record: AccountRecord) -> Optional[int]:
    value = record.metadata.get(COOLDOWN_UNTIL_KEY)
    if value in (None, ""):
        return None


def refresh_due_at(
    record: AccountRecord,
    *,
    interval_ms: int,
) -> Optional[int]:
    if derive_state(record) != AccountLifecycleState.COOLING:
        return None
    due = cooldown_until(record)
    if due is not None:
        return due
    if record.last_sync_at is None:
        return 0
    return int(record.last_sync_at) + max(0, int(interval_ms))


def needs_refresh(
    record: AccountRecord,
    *,
    now: Optional[int] = None,
    interval_ms: int,
) -> bool:
    due = refresh_due_at(record, interval_ms=interval_ms)
    if due is None:
        return False
    current = now_ms() if now is None else now
    return due <= current


def is_selectable(
    record: AccountRecord,
    *,
    consumed_mode: bool = False,
) -> bool:
    state = derive_state(record)
    if state != AccountLifecycleState.ACTIVE:
        return False
    if consumed_mode:
        return True
    return record.quota > 0


def _clear_metadata_keys(record: AccountRecord, *keys: str) -> None:
    for key in keys:
        record.metadata.pop(key, None)


def _normalize_after_success(record: AccountRecord, *, consumed_mode: bool) -> None:
    if derive_state(record) == AccountLifecycleState.DELETED:
        return
    if record.status in (AccountStatus.DISABLED, AccountStatus.EXPIRED):
        if consumed_mode or record.quota > 0:
            record.status = AccountStatus.ACTIVE
        return
    if consumed_mode or record.quota > 0:
        record.status = AccountStatus.ACTIVE
    else:
        record.status = AccountStatus.COOLING


def apply_feedback(
    record: AccountRecord,
    feedback: AccountFeedback,
    *,
    policy: Optional[AccountStatePolicy] = None,
) -> AccountRecord:
    policy = policy or AccountStatePolicy()
    updated = record.model_copy(deep=True)
    at = feedback.at
    updated.updated_at = at

    if feedback.status_code is not None:
        updated.metadata[LAST_STATUS_CODE_KEY] = feedback.status_code

    if feedback.kind == AccountFeedbackKind.DELETE:
        updated.deleted_at = at
        return updated

    if feedback.kind == AccountFeedbackKind.RESTORE:
        updated.deleted_at = None
        updated.fail_count = 0
        updated.last_fail_at = None
        updated.last_fail_reason = None
        _clear_metadata_keys(
            updated,
            COOLDOWN_UNTIL_KEY,
            COOLDOWN_REASON_KEY,
            COOLDOWN_STARTED_AT_KEY,
            DISABLED_AT_KEY,
            DISABLED_REASON_KEY,
            EXPIRED_AT_KEY,
            EXPIRED_REASON_KEY,
        )
        _normalize_after_success(updated, consumed_mode=feedback.consumed_mode)
        return updated

    if derive_state(updated) == AccountLifecycleState.DELETED:
        return updated

    if feedback.kind == AccountFeedbackKind.DISABLE:
        updated.status = AccountStatus.DISABLED
        updated.last_fail_at = at
        updated.last_fail_reason = feedback.reason or "disabled"
        updated.metadata[DISABLED_AT_KEY] = at
        updated.metadata[DISABLED_REASON_KEY] = updated.last_fail_reason
        return updated

    if feedback.kind == AccountFeedbackKind.SUCCESS:
        if feedback.quota_remaining is not None:
            updated.quota = max(0, int(feedback.quota_remaining))
        if feedback.apply_usage:
            cost = effort_cost(feedback.effort)
            updated.last_used_at = at
            updated.consumed += cost
            if feedback.consumed_mode:
                updated.use_count += 1
            else:
                actual_cost = cost
                if feedback.quota_remaining is None:
                    actual_cost = min(cost, max(updated.quota, 0))
                    updated.quota = max(0, updated.quota - actual_cost)
                updated.use_count += actual_cost
        updated.fail_count = 0
        updated.last_fail_at = None
        updated.last_fail_reason = None
        updated.metadata[LAST_RECOVERED_AT_KEY] = at
        _clear_metadata_keys(
            updated,
            COOLDOWN_UNTIL_KEY,
            COOLDOWN_REASON_KEY,
            COOLDOWN_STARTED_AT_KEY,
            DISABLED_AT_KEY,
            DISABLED_REASON_KEY,
            EXPIRED_AT_KEY,
            EXPIRED_REASON_KEY,
        )
        _normalize_after_success(updated, consumed_mode=feedback.consumed_mode)
        return updated

    updated.last_fail_at = at
    updated.last_fail_reason = feedback.reason or feedback.kind.value

    if feedback.kind == AccountFeedbackKind.UNAUTHORIZED:
        updated.fail_count += 1
        if feedback.confirm_expired or updated.fail_count >= policy.failure_threshold:
            updated.status = AccountStatus.EXPIRED
            updated.metadata[EXPIRED_AT_KEY] = at
            updated.metadata[EXPIRED_REASON_KEY] = updated.last_fail_reason
        return updated

    if feedback.kind == AccountFeedbackKind.FORBIDDEN:
        updated.fail_count += 1
        if updated.fail_count >= policy.forbidden_threshold:
            updated.status = AccountStatus.DISABLED
            updated.metadata[DISABLED_AT_KEY] = at
            updated.metadata[DISABLED_REASON_KEY] = updated.last_fail_reason
        return updated

    if feedback.kind == AccountFeedbackKind.RATE_LIMITED:
        updated.status = AccountStatus.COOLING
        updated.quota = 0 if feedback.quota_remaining is None else max(
            0, int(feedback.quota_remaining)
        )
        retry_after_ms = feedback.retry_after_ms
        if retry_after_ms is None:
            retry_after_ms = policy.default_cooling_ms
        updated.metadata[COOLDOWN_STARTED_AT_KEY] = at
        updated.metadata[COOLDOWN_UNTIL_KEY] = at + max(0, retry_after_ms)
        updated.metadata[COOLDOWN_REASON_KEY] = updated.last_fail_reason
        return updated

    return updated
