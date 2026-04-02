"""
Management and orchestration services for the account domain.
"""

from __future__ import annotations

from typing import Optional, Sequence

from app.services.account.commands import (
    AccountPatch,
    AccountUpsert,
    BulkReplacePoolCommand,
    ListAccountsQuery,
)
from app.services.account.models import AccountMutationResult, AccountPage, AccountRecord, EffortType
from app.services.account.repository import AccountRepository
from app.services.account.runtime import AccountDirectory, AccountLease
from app.services.account.state_machine import (
    AccountFeedback,
    AccountLifecycleState,
    apply_feedback,
)


class AccountManagementService:
    def __init__(self, repository: AccountRepository):
        self.repository = repository

    async def list_accounts(self, query: Optional[ListAccountsQuery] = None) -> AccountPage:
        return await self.repository.list_accounts(query or ListAccountsQuery())

    async def bulk_upsert(self, items: Sequence[AccountUpsert]) -> AccountMutationResult:
        return await self.repository.upsert_accounts(items)

    async def bulk_patch(self, patches: Sequence[AccountPatch]) -> AccountMutationResult:
        return await self.repository.patch_accounts(patches)

    async def bulk_delete(self, tokens: Sequence[str]) -> AccountMutationResult:
        return await self.repository.delete_accounts(tokens)

    async def replace_pool(self, command: BulkReplacePoolCommand) -> AccountMutationResult:
        return await self.repository.replace_pool(command.pool_name, command.items)


class RuntimeAccountService:
    def __init__(self, repository: AccountRepository, directory: Optional[AccountDirectory] = None):
        self.repository = repository
        self.directory = directory or AccountDirectory(repository)

    async def bootstrap(self) -> None:
        await self.directory.bootstrap()

    async def refresh_if_changed(self) -> bool:
        return await self.directory.refresh_if_changed()

    async def reserve_account(
        self,
        pool_names: str | Sequence[str],
        *,
        exclude: Optional[set[str]] = None,
        prefer_tags: Optional[set[str]] = None,
        consumed_mode: bool = False,
        effort: EffortType = EffortType.LOW,
    ) -> Optional[AccountLease]:
        return await self.directory.reserve(
            pool_names,
            exclude=exclude,
            prefer_tags=prefer_tags,
            consumed_mode=consumed_mode,
            effort=effort,
        )

    async def release_account(self, lease_id: str) -> bool:
        return await self.directory.release(lease_id)

    async def select_account(
        self,
        pool_names: str | Sequence[str],
        *,
        exclude: Optional[set[str]] = None,
        prefer_tags: Optional[set[str]] = None,
        consumed_mode: bool = False,
        effort: EffortType = EffortType.LOW,
    ):
        return await self.directory.select(
            pool_names,
            exclude=exclude,
            prefer_tags=prefer_tags,
            consumed_mode=consumed_mode,
            effort=effort,
        )

    def get_account(self, token: str):
        return self.directory.get_record(token)

    def list_runtime_accounts(
        self,
        *,
        pool_names: Optional[set[str]] = None,
        states: Optional[set[AccountLifecycleState]] = None,
    ) -> list[AccountRecord]:
        return self.directory.list_records(pool_names=pool_names, states=states)

    async def upsert_runtime_record(
        self,
        record: AccountRecord,
        *,
        persist: bool = True,
    ) -> AccountRecord:
        updated = await self.directory.upsert_record(record)
        if persist:
            await self.repository.upsert_accounts([AccountUpsert.from_record(updated)])
        return updated

    async def apply_feedback(
        self,
        feedback: AccountFeedback,
        *,
        token: Optional[str] = None,
        lease_id: Optional[str] = None,
        persist: bool = True,
    ):
        updated = await self.directory.apply_feedback(
            feedback,
            token=token,
            lease_id=lease_id,
            apply_state=apply_feedback,
        )
        if updated and persist:
            await self.repository.upsert_accounts([AccountUpsert.from_record(updated)])
        return updated
