"""
Bridges request-chain feedback into the new account domain.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Any, Optional, Sequence

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.account.factory import (
    AccountRepositorySettings,
    create_account_repository,
)
from app.services.account.models import EffortType
from app.services.account.refresh import AccountRefreshService
from app.services.account.repository import AccountRepository
from app.services.account.service import AccountManagementService, RuntimeAccountService
from app.services.account.state_machine import AccountFeedback, AccountFeedbackKind
from app.services.reverse.utils.retry import extract_retry_after


@dataclass(slots=True)
class AccountDomainContext:
    repository: AccountRepository
    runtime_service: RuntimeAccountService
    refresh_service: AccountRefreshService


_context: Optional[AccountDomainContext] = None
_context_lock = asyncio.Lock()


def _normalize_effort(value: Any) -> EffortType:
    if isinstance(value, EffortType):
        return value
    raw = getattr(value, "value", value)
    if str(raw).lower() == EffortType.HIGH.value:
        return EffortType.HIGH
    return EffortType.LOW


def _normalize_pool_names(pool_names: str | Sequence[str]) -> list[str]:
    if isinstance(pool_names, str):
        pool_names = [pool_names]
    return [str(pool).strip() for pool in pool_names if str(pool).strip()]


async def get_account_domain_context(
    settings: Optional[AccountRepositorySettings] = None,
) -> AccountDomainContext:
    global _context
    if _context is not None:
        return _context
    async with _context_lock:
        if _context is not None:
            return _context
        repository = create_account_repository(settings)
        await repository.initialize()
        runtime_service = RuntimeAccountService(repository)
        await runtime_service.bootstrap()
        refresh_service = AccountRefreshService(runtime_service)
        _context = AccountDomainContext(
            repository=repository,
            runtime_service=runtime_service,
            refresh_service=refresh_service,
        )
        return _context


class AccountFeedbackCoordinator:
    def __init__(
        self,
        runtime_service: RuntimeAccountService,
        *,
        refresh_service: Optional[AccountRefreshService] = None,
    ):
        self.runtime_service = runtime_service
        self.refresh_service = refresh_service

    async def select_token(
        self,
        pool_names: str | Sequence[str],
        *,
        exclude: Optional[set[str]] = None,
        prefer_tags: Optional[set[str]] = None,
        effort: Any = EffortType.LOW,
    ) -> Optional[str]:
        normalized_pools = _normalize_pool_names(pool_names)
        if not normalized_pools:
            return None
        record = await self.runtime_service.select_account(
            normalized_pools,
            exclude=exclude,
            prefer_tags=prefer_tags,
            effort=_normalize_effort(effort),
        )
        if record is not None:
            return record.token
        await self.runtime_service.refresh_if_changed()
        record = await self.runtime_service.select_account(
            normalized_pools,
            exclude=exclude,
            prefer_tags=prefer_tags,
            effort=_normalize_effort(effort),
        )
        if record is None:
            return None
        return record.token

    async def _apply(self, token: str, feedback: AccountFeedback) -> bool:
        updated = await self.runtime_service.apply_feedback(feedback, token=token)
        if updated is not None:
            return True
        await self.runtime_service.refresh_if_changed()
        updated = await self.runtime_service.apply_feedback(feedback, token=token)
        return updated is not None

    async def report_success(
        self,
        token: str,
        *,
        effort: Any = EffortType.LOW,
    ) -> bool:
        return await self._apply(
            token,
            AccountFeedback(
                kind=AccountFeedbackKind.SUCCESS,
                effort=_normalize_effort(effort),
                apply_usage=True,
            ),
        )

    async def report_status(
        self,
        token: str,
        status_code: int,
        *,
        reason: str = "",
        confirm_expired: bool = False,
        retry_after_ms: Optional[int] = None,
        include_unauthorized: bool = True,
    ) -> bool:
        if status_code == 401 and not include_unauthorized:
            return False
        feedback = AccountFeedback.from_status_code(
            status_code,
            reason=reason,
            retry_after_ms=retry_after_ms,
            confirm_expired=confirm_expired,
            apply_usage=False,
        )
        if feedback.kind.value == "success":
            return False
        return await self._apply(token, feedback)

    async def report_rate_limited(
        self,
        token: str,
        *,
        reason: str = "rate_limited",
        retry_after_ms: Optional[int] = None,
    ) -> bool:
        return await self.report_status(
            token,
            429,
            reason=reason,
            retry_after_ms=retry_after_ms,
            include_unauthorized=False,
        )

    async def report_upstream_exception(
        self,
        token: str,
        error: Exception,
        *,
        reason: str = "",
        include_unauthorized: bool = False,
    ) -> bool:
        if not isinstance(error, UpstreamException):
            return False
        details = error.details if isinstance(error.details, dict) else {}
        status = details.get("status", getattr(error, "status_code", None))
        if status is None:
            return False
        retry_after = extract_retry_after(error)
        return await self.report_status(
            token,
            int(status),
            reason=reason or str(details.get("error_code") or details.get("error") or ""),
            confirm_expired=bool(details.get("is_token_expired", False)),
            retry_after_ms=int(retry_after * 1000) if retry_after else None,
            include_unauthorized=include_unauthorized,
        )

    async def refresh_on_demand(self):
        if self.refresh_service is None:
            return None
        return await self.refresh_service.refresh_due_accounts_on_demand()

    async def refresh_tokens(self, tokens: list[str]):
        if self.refresh_service is None:
            return None
        return await self.refresh_service.refresh_accounts(tokens, trigger="manual")


async def get_account_feedback_coordinator() -> AccountFeedbackCoordinator:
    context = await get_account_domain_context()
    return AccountFeedbackCoordinator(
        context.runtime_service,
        refresh_service=context.refresh_service,
    )


async def get_account_management_service() -> AccountManagementService:
    context = await get_account_domain_context()
    return AccountManagementService(context.repository)


async def maybe_get_account_feedback_coordinator() -> Optional[AccountFeedbackCoordinator]:
    try:
        return await get_account_feedback_coordinator()
    except Exception as error:
        logger.debug("Account coordinator unavailable: {}", error)
        return None


async def safe_account_feedback(call, *args, **kwargs) -> bool:
    try:
        return bool(await call(*args, **kwargs))
    except Exception as error:
        logger.debug("Account feedback skipped: {}", error)
        return False


__all__ = [
    "AccountDomainContext",
    "AccountFeedbackCoordinator",
    "get_account_domain_context",
    "get_account_feedback_coordinator",
    "get_account_management_service",
    "maybe_get_account_feedback_coordinator",
    "safe_account_feedback",
]
