"""Account-domain token facade."""

from typing import Any, Dict, Optional, Sequence, Set

from app.services.account.commands import AccountPatch, AccountUpsert, ListAccountsQuery
from app.services.account.models import AccountRecord, AccountStatus, EffortType
from app.services.account.state_machine import (
    COOLDOWN_REASON_KEY,
    COOLDOWN_STARTED_AT_KEY,
    COOLDOWN_UNTIL_KEY,
    DISABLED_AT_KEY,
    DISABLED_REASON_KEY,
    EXPIRED_AT_KEY,
    EXPIRED_REASON_KEY,
)

BASIC_DEFAULT_QUOTA = 80
SUPER_DEFAULT_QUOTA = 140


class TokenService:
    """Thin facade over the account domain."""

    @staticmethod
    async def _get_account_context():
        from app.services.account.coordinator import get_account_domain_context

        return await get_account_domain_context()

    @staticmethod
    async def _get_management_service():
        from app.services.account.coordinator import get_account_management_service

        return await get_account_management_service()

    @staticmethod
    def _default_quota_for_pool(pool_name: str) -> int:
        if pool_name == "ssoSuper":
            return SUPER_DEFAULT_QUOTA
        return BASIC_DEFAULT_QUOTA

    @staticmethod
    def _account_to_token_payload(record: AccountRecord) -> dict[str, Any]:
        return {
            "token": record.token,
            "status": record.status.value,
            "quota": record.quota,
            "consumed": record.consumed,
            "created_at": record.created_at,
            "last_used_at": record.last_used_at,
            "use_count": record.use_count,
            "fail_count": record.fail_count,
            "last_fail_at": record.last_fail_at,
            "last_fail_reason": record.last_fail_reason,
            "last_sync_at": record.last_sync_at,
            "tags": list(record.tags),
            "note": record.note,
            "last_asset_clear_at": record.last_asset_clear_at,
        }

    @staticmethod
    def _empty_pool_stats() -> dict[str, Any]:
        return {
            "total": 0,
            "active": 0,
            "disabled": 0,
            "expired": 0,
            "cooling": 0,
            "total_quota": 0,
            "avg_quota": 0.0,
            "total_consumed": 0,
            "avg_consumed": 0.0,
        }

    @staticmethod
    async def _list_all_accounts(*, include_deleted: bool = False) -> list[AccountRecord]:
        service = await TokenService._get_management_service()
        page = 1
        records: list[AccountRecord] = []
        while True:
            result = await service.list_accounts(
                ListAccountsQuery(
                    page=page,
                    page_size=2000,
                    include_deleted=include_deleted,
                )
            )
            records.extend(result.items)
            if page >= result.total_pages:
                break
            page += 1
        return records

    @staticmethod
    async def get_token(pool_name: str = "ssoBasic") -> Optional[str]:
        return await TokenService.select_token([pool_name])

    @staticmethod
    async def select_token(
        pool_names: Sequence[str],
        *,
        exclude: Optional[Set[str]] = None,
        prefer_tags: Optional[Set[str]] = None,
        effort: EffortType = EffortType.LOW,
    ) -> Optional[str]:
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return await coordinator.select_token(
            pool_names,
            exclude=exclude,
            prefer_tags=prefer_tags,
            effort=effort,
        )

    @staticmethod
    async def get_account(token: str):
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return coordinator.runtime_service.get_account(token)

    @staticmethod
    async def refresh_tokens_on_demand():
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return await coordinator.refresh_on_demand()

    @staticmethod
    async def consume(token: str, effort: EffortType = EffortType.LOW) -> bool:
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return await coordinator.report_success(token, effort=effort)

    @staticmethod
    async def sync_usage(token: str, effort: EffortType = EffortType.LOW) -> bool:
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        result = await coordinator.refresh_tokens([token])
        return bool(result and result.checked > 0 and result.failed == 0)

    @staticmethod
    async def record_fail(token: str, status_code: int = 401, reason: str = "") -> bool:
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return await coordinator.report_status(
            token,
            status_code,
            reason=reason,
            confirm_expired=status_code == 401 and "expired" in reason.lower(),
        )

    @staticmethod
    async def mark_rate_limited(token: str, reason: str = "rate_limited") -> bool:
        from app.services.account.coordinator import get_account_feedback_coordinator

        coordinator = await get_account_feedback_coordinator()
        return await coordinator.report_rate_limited(token, reason=reason)

    @staticmethod
    async def add_token(token: str, pool_name: str = "ssoBasic") -> bool:
        service = await TokenService._get_management_service()
        result = await service.bulk_upsert(
            [AccountUpsert(token=token, pool_name=pool_name)]
        )
        context = await TokenService._get_account_context()
        await context.runtime_service.refresh_if_changed()
        return result.upserted > 0

    @staticmethod
    async def remove_token(token: str) -> bool:
        service = await TokenService._get_management_service()
        result = await service.bulk_delete([token])
        context = await TokenService._get_account_context()
        await context.runtime_service.refresh_if_changed()
        return result.deleted > 0

    @staticmethod
    async def reset_token(token: str) -> bool:
        context = await TokenService._get_account_context()
        record = context.runtime_service.get_account(token)
        if record is None:
            await context.runtime_service.refresh_if_changed()
            record = context.runtime_service.get_account(token)
        if record is None:
            return False

        service = await TokenService._get_management_service()
        result = await service.bulk_patch(
            [
                AccountPatch(
                    token=record.token,
                    status=AccountStatus.ACTIVE,
                    quota=TokenService._default_quota_for_pool(record.pool_name),
                    consumed=0,
                    clear_failures=True,
                    metadata_merge={
                        COOLDOWN_UNTIL_KEY: None,
                        COOLDOWN_REASON_KEY: None,
                        COOLDOWN_STARTED_AT_KEY: None,
                        DISABLED_AT_KEY: None,
                        DISABLED_REASON_KEY: None,
                        EXPIRED_AT_KEY: None,
                        EXPIRED_REASON_KEY: None,
                    },
                )
            ]
        )
        await context.runtime_service.refresh_if_changed()
        return result.patched > 0

    @staticmethod
    async def reset_all():
        service = await TokenService._get_management_service()
        records = await TokenService._list_all_accounts(include_deleted=False)
        patches = [
            AccountPatch(
                token=record.token,
                status=AccountStatus.ACTIVE,
                quota=TokenService._default_quota_for_pool(record.pool_name),
                consumed=0,
                clear_failures=True,
                metadata_merge={
                    COOLDOWN_UNTIL_KEY: None,
                    COOLDOWN_REASON_KEY: None,
                    COOLDOWN_STARTED_AT_KEY: None,
                    DISABLED_AT_KEY: None,
                    DISABLED_REASON_KEY: None,
                    EXPIRED_AT_KEY: None,
                    EXPIRED_REASON_KEY: None,
                },
            )
            for record in records
        ]
        if patches:
            await service.bulk_patch(patches)
        context = await TokenService._get_account_context()
        await context.runtime_service.refresh_if_changed()

    @staticmethod
    async def get_stats() -> Dict[str, dict]:
        records = await TokenService._list_all_accounts(include_deleted=False)
        stats: Dict[str, dict] = {}
        for record in records:
            pool_stats = stats.setdefault(record.pool_name, TokenService._empty_pool_stats())
            pool_stats["total"] += 1
            pool_stats["total_quota"] += max(0, record.quota)
            pool_stats["total_consumed"] += max(0, record.consumed)
            status = record.status.value
            if status in {"active", "disabled", "expired", "cooling"}:
                pool_stats[status] += 1
        for pool_stats in stats.values():
            total = pool_stats["total"]
            if total > 0:
                pool_stats["avg_quota"] = pool_stats["total_quota"] / total
                pool_stats["avg_consumed"] = pool_stats["total_consumed"] / total
        return stats

    @staticmethod
    async def list_tokens(pool_name: str = "ssoBasic") -> list[dict[str, Any]]:
        records = await TokenService._list_all_accounts(include_deleted=False)
        return [
            TokenService._account_to_token_payload(record)
            for record in records
            if record.pool_name == pool_name
        ]

    @staticmethod
    async def add_tag(token: str, tag: str) -> bool:
        normalized_tag = str(tag or "").strip()
        if not normalized_tag:
            return False
        service = await TokenService._get_management_service()
        result = await service.bulk_patch(
            [AccountPatch(token=token, add_tags=[normalized_tag])]
        )
        context = await TokenService._get_account_context()
        await context.runtime_service.refresh_if_changed()
        return result.patched > 0

    @staticmethod
    async def mark_asset_clear(token: str) -> bool:
        from app.services.account.models import now_ms

        service = await TokenService._get_management_service()
        result = await service.bulk_patch(
            [AccountPatch(token=token, last_asset_clear_at=now_ms())]
        )
        context = await TokenService._get_account_context()
        await context.runtime_service.refresh_if_changed()
        return result.patched > 0


__all__ = [
    "BASIC_DEFAULT_QUOTA",
    "SUPER_DEFAULT_QUOTA",
    "TokenService",
]
