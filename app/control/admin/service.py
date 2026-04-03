"""Admin service layer — aggregates token CRUD and proxy management.

Provides a service-level API that admin handlers call instead of
reaching directly into repositories.  Centralises validation, audit
logging, and cross-cutting concerns.
"""

from __future__ import annotations

from typing import Any

from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_ms
from app.control.account.commands import (
    AccountPatch, AccountUpsert, BulkReplacePoolCommand, ListAccountsQuery,
)
from app.control.account.models import (
    AccountMutationResult, AccountPage, AccountRecord,
)
from app.control.account.repository import AccountRepository


class AccountAdminService:
    """High-level account administration operations."""

    def __init__(self, repo: AccountRepository) -> None:
        self._repo = repo

    async def list_accounts(self, query: ListAccountsQuery) -> AccountPage:
        return await self._repo.list_accounts(query)

    async def get_accounts(self, tokens: list[str]) -> list[AccountRecord]:
        return await self._repo.get_accounts(tokens)

    async def upsert(self, items: list[AccountUpsert]) -> AccountMutationResult:
        result = await self._repo.upsert_accounts(items)
        logger.info("Admin upsert: {} accounts affected", result.upserted)
        return result

    async def delete(self, tokens: list[str]) -> AccountMutationResult:
        result = await self._repo.delete_accounts(tokens)
        logger.info("Admin delete: {} accounts affected", result.deleted)
        return result

    async def patch(self, patches: list[AccountPatch]) -> AccountMutationResult:
        result = await self._repo.patch_accounts(patches)
        logger.info("Admin patch: {} accounts affected", result.patched)
        return result

    async def replace_pool(self, command: BulkReplacePoolCommand) -> AccountMutationResult:
        result = await self._repo.replace_pool(command)
        logger.info(
            "Admin replace_pool({}): upserted={} deleted={}",
            command.pool, result.upserted, result.deleted,
        )
        return result


class ProxyAdminService:
    """High-level proxy administration operations."""

    def __init__(self) -> None:
        self._directory = None

    async def _get_directory(self):
        if self._directory is None:
            from app.control.proxy import get_proxy_directory
            self._directory = await get_proxy_directory()
        return self._directory

    async def status(self) -> dict[str, Any]:
        """Return current proxy status summary."""
        d = await self._get_directory()
        return {
            "egress_mode":    d.egress_mode.value,
            "clearance_mode": d.clearance_mode.value,
            "node_count":     d.node_count,
        }

    async def reload(self) -> dict[str, Any]:
        """Reload proxy configuration."""
        d = await self._get_directory()
        await d.load()
        logger.info("Admin proxy reload complete")
        return await self.status()


__all__ = ["AccountAdminService", "ProxyAdminService"]
