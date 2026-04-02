"""
Repository contract for high-throughput account persistence backends.
"""

from __future__ import annotations

import abc
from typing import Sequence

from app.services.account.commands import AccountPatch, AccountUpsert, ListAccountsQuery
from app.services.account.models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountPage,
    AccountRecord,
    RuntimeSnapshot,
)


class AccountRepository(abc.ABC):
    @abc.abstractmethod
    async def initialize(self) -> None:
        """Prepare schema/indexes/resources."""

    @abc.abstractmethod
    async def close(self) -> None:
        """Release connections/resources."""

    @abc.abstractmethod
    async def get_revision(self) -> int:
        """Return the latest committed repository revision."""

    @abc.abstractmethod
    async def get_accounts(self, tokens: Sequence[str]) -> dict[str, AccountRecord]:
        """Fetch account rows by token."""

    @abc.abstractmethod
    async def list_accounts(self, query: ListAccountsQuery) -> AccountPage:
        """List accounts for management workflows."""

    @abc.abstractmethod
    async def upsert_accounts(
        self, items: Sequence[AccountUpsert]
    ) -> AccountMutationResult:
        """Insert or replace accounts by token."""

    @abc.abstractmethod
    async def patch_accounts(
        self, patches: Sequence[AccountPatch]
    ) -> AccountMutationResult:
        """Patch account rows in place."""

    @abc.abstractmethod
    async def delete_accounts(self, tokens: Sequence[str]) -> AccountMutationResult:
        """Soft-delete account rows and emit tombstones for runtime caches."""

    @abc.abstractmethod
    async def scan_changes(
        self, since_revision: int, *, limit: int = 5000
    ) -> AccountChangeSet:
        """Return upserts/tombstones newer than the provided revision."""

    @abc.abstractmethod
    async def runtime_snapshot(
        self, *, include_deleted: bool = False
    ) -> RuntimeSnapshot:
        """Return a full snapshot optimized for runtime cache bootstrap."""

    async def replace_pool(
        self,
        pool_name: str,
        items: Sequence[AccountUpsert],
    ) -> AccountMutationResult:
        """
        Default pool-replacement implementation.

        Backends can override this to do it more efficiently.
        """
        existing_tokens: set[str] = set()
        page = 1
        while True:
            current = await self.list_accounts(
                ListAccountsQuery(
                    page=page,
                    page_size=2000,
                    pool_names=[pool_name],
                    include_deleted=False,
                )
            )
            existing_tokens.update(item.token for item in current.items)
            if page >= current.total_pages:
                break
            page += 1
        incoming_tokens = {item.token for item in items}
        result = await self.upsert_accounts(items)
        removed = list(existing_tokens - incoming_tokens)
        if removed:
            delete_result = await self.delete_accounts(removed)
            result.deleted += delete_result.deleted
            result.revision = max(result.revision, delete_result.revision)
        return result

    async def get_metadata(self) -> dict[str, str]:
        """Optional backend metadata such as schema version and migration markers."""
        return {}

    async def set_metadata(self, mapping: dict[str, str]) -> None:
        """Optional backend metadata update hook."""
        return None


class NullAccountRepository(AccountRepository):
    async def initialize(self) -> None:
        return None

    async def close(self) -> None:
        return None

    async def get_revision(self) -> int:
        return 0

    async def get_accounts(self, tokens: Sequence[str]) -> dict[str, AccountRecord]:
        return {}

    async def list_accounts(self, query: ListAccountsQuery) -> AccountPage:
        return AccountPage()

    async def upsert_accounts(
        self, items: Sequence[AccountUpsert]
    ) -> AccountMutationResult:
        return AccountMutationResult()

    async def patch_accounts(
        self, patches: Sequence[AccountPatch]
    ) -> AccountMutationResult:
        return AccountMutationResult()

    async def delete_accounts(self, tokens: Sequence[str]) -> AccountMutationResult:
        return AccountMutationResult()

    async def scan_changes(
        self, since_revision: int, *, limit: int = 5000
    ) -> AccountChangeSet:
        return AccountChangeSet(revision=since_revision)

    async def runtime_snapshot(
        self, *, include_deleted: bool = False
    ) -> RuntimeSnapshot:
        return RuntimeSnapshot()
