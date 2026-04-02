"""
Account domain models.
"""

from __future__ import annotations

from datetime import datetime
from enum import Enum
from typing import Any, Optional

from pydantic import BaseModel, Field, field_validator


def now_ms() -> int:
    return int(datetime.now().timestamp() * 1000)


class AccountStatus(str, Enum):
    ACTIVE = "active"
    COOLING = "cooling"
    EXPIRED = "expired"
    DISABLED = "disabled"


class EffortType(str, Enum):
    LOW = "low"
    HIGH = "high"


class AccountSortField(str, Enum):
    UPDATED_AT = "updated_at"
    CREATED_AT = "created_at"
    LAST_USED_AT = "last_used_at"
    QUOTA = "quota"
    CONSUMED = "consumed"
    USE_COUNT = "use_count"
    TOKEN = "token"


class SortDirection(str, Enum):
    ASC = "asc"
    DESC = "desc"


class AccountRecord(BaseModel):
    """
    Persisted account row.

    One account maps to one token. All stores use the same logical row shape.
    """

    token: str
    pool_name: str
    status: AccountStatus = AccountStatus.ACTIVE
    quota: int = 80
    consumed: int = 0
    created_at: int = Field(default_factory=now_ms)
    updated_at: int = Field(default_factory=now_ms)
    last_used_at: Optional[int] = None
    use_count: int = 0
    fail_count: int = 0
    last_fail_at: Optional[int] = None
    last_fail_reason: Optional[str] = None
    last_sync_at: Optional[int] = None
    tags: list[str] = Field(default_factory=list)
    note: str = ""
    last_asset_clear_at: Optional[int] = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    deleted_at: Optional[int] = None

    @field_validator("token", mode="before")
    @classmethod
    def normalize_token(cls, value: Any) -> str:
        if value is None:
            raise ValueError("token cannot be empty")
        token = str(value)
        token = token.translate(
            str.maketrans(
                {
                    "\u2010": "-",
                    "\u2011": "-",
                    "\u2012": "-",
                    "\u2013": "-",
                    "\u2014": "-",
                    "\u2212": "-",
                    "\u00a0": " ",
                    "\u2007": " ",
                    "\u202f": " ",
                    "\u200b": "",
                    "\u200c": "",
                    "\u200d": "",
                    "\ufeff": "",
                }
            )
        )
        token = "".join(token.split())
        if token.startswith("sso="):
            token = token[4:]
        token = token.encode("ascii", errors="ignore").decode("ascii")
        if not token:
            raise ValueError("token cannot be empty")
        return token

    @field_validator("pool_name", mode="before")
    @classmethod
    def normalize_pool_name(cls, value: Any) -> str:
        pool_name = str(value or "").strip()
        if not pool_name:
            raise ValueError("pool_name cannot be empty")
        return pool_name

    @field_validator("tags", mode="before")
    @classmethod
    def normalize_tags(cls, value: Any) -> list[str]:
        if not value:
            return []
        tags: list[str] = []
        if isinstance(value, str):
            value = [part.strip() for part in value.split(",")]
        for item in value:
            if item is None:
                continue
            tag = str(item).strip()
            if tag and tag not in tags:
                tags.append(tag)
        return tags

    def is_deleted(self) -> bool:
        return self.deleted_at is not None

    def is_selectable(self, *, consumed_mode: bool = False) -> bool:
        if self.is_deleted():
            return False
        if self.status != AccountStatus.ACTIVE:
            return False
        if consumed_mode:
            return True
        return self.quota > 0


class AccountMutationResult(BaseModel):
    upserted: int = 0
    patched: int = 0
    deleted: int = 0
    revision: int = 0


class AccountSummary(BaseModel):
    total: int = 0
    active: int = 0
    cooling: int = 0
    expired: int = 0
    disabled: int = 0
    deleted: int = 0
    nsfw: int = 0
    no_nsfw: int = 0
    chat_quota: int = 0
    image_quota: int = 0
    total_consumed: int = 0
    total_calls: int = 0


class AccountPage(BaseModel):
    items: list[AccountRecord] = Field(default_factory=list)
    total: int = 0
    page: int = 1
    page_size: int = 50
    total_pages: int = 1
    summary: AccountSummary = Field(default_factory=AccountSummary)
    revision: int = 0


class AccountChangeSet(BaseModel):
    revision: int = 0
    items: list[AccountRecord] = Field(default_factory=list)
    deleted_tokens: list[str] = Field(default_factory=list)
    has_more: bool = False


class RuntimeSnapshot(BaseModel):
    revision: int = 0
    items: list[AccountRecord] = Field(default_factory=list)
