"""
Query and mutation models for the account domain.
"""

from __future__ import annotations

from typing import Any, Optional

from pydantic import BaseModel, Field, field_validator

from app.services.account.models import (
    AccountRecord,
    AccountSortField,
    AccountStatus,
    SortDirection,
    now_ms,
)


class ListAccountsQuery(BaseModel):
    page: int = Field(1, ge=1)
    page_size: int = Field(50, ge=1, le=2000)
    pool_names: list[str] = Field(default_factory=list)
    statuses: list[AccountStatus] = Field(default_factory=list)
    tags_all: list[str] = Field(default_factory=list)
    search: str = ""
    include_deleted: bool = False
    sort_field: AccountSortField = AccountSortField.CREATED_AT
    sort_direction: SortDirection = SortDirection.DESC

    @field_validator("pool_names", "tags_all", mode="before")
    @classmethod
    def normalize_string_list(cls, value: Any) -> list[str]:
        if not value:
            return []
        if isinstance(value, str):
            value = [part.strip() for part in value.split(",")]
        return [str(item).strip() for item in value if str(item).strip()]


class AccountUpsert(BaseModel):
    token: str
    pool_name: str
    status: AccountStatus = AccountStatus.ACTIVE
    quota: int = 80
    consumed: int = 0
    created_at: Optional[int] = None
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

    @classmethod
    def from_record(cls, record: AccountRecord) -> "AccountUpsert":
        return cls(
            token=record.token,
            pool_name=record.pool_name,
            status=record.status,
            quota=record.quota,
            consumed=record.consumed,
            created_at=record.created_at,
            last_used_at=record.last_used_at,
            use_count=record.use_count,
            fail_count=record.fail_count,
            last_fail_at=record.last_fail_at,
            last_fail_reason=record.last_fail_reason,
            last_sync_at=record.last_sync_at,
            tags=list(record.tags),
            note=record.note,
            last_asset_clear_at=record.last_asset_clear_at,
            metadata=dict(record.metadata),
        )

    def to_record(self, *, current: Optional[AccountRecord] = None, revision: int) -> AccountRecord:
        if current is None:
            created_at = self.created_at or revision
            return AccountRecord(
                token=self.token,
                pool_name=self.pool_name,
                status=self.status,
                quota=self.quota,
                consumed=self.consumed,
                created_at=created_at,
                updated_at=revision,
                last_used_at=self.last_used_at,
                use_count=self.use_count,
                fail_count=self.fail_count,
                last_fail_at=self.last_fail_at,
                last_fail_reason=self.last_fail_reason,
                last_sync_at=self.last_sync_at,
                tags=self.tags,
                note=self.note,
                last_asset_clear_at=self.last_asset_clear_at,
                metadata=self.metadata,
                deleted_at=None,
            )

        merged = current.model_copy(deep=True)
        merged.pool_name = self.pool_name
        merged.status = self.status
        merged.quota = self.quota
        merged.consumed = self.consumed
        merged.last_used_at = self.last_used_at
        merged.use_count = self.use_count
        merged.fail_count = self.fail_count
        merged.last_fail_at = self.last_fail_at
        merged.last_fail_reason = self.last_fail_reason
        merged.last_sync_at = self.last_sync_at
        merged.tags = list(self.tags)
        merged.note = self.note
        merged.last_asset_clear_at = self.last_asset_clear_at
        merged.metadata = dict(self.metadata)
        merged.updated_at = revision
        merged.deleted_at = None
        return merged


class AccountPatch(BaseModel):
    token: str
    pool_name: Optional[str] = None
    status: Optional[AccountStatus] = None
    quota: Optional[int] = None
    consumed: Optional[int] = None
    last_used_at: Optional[int] = None
    use_count: Optional[int] = None
    fail_count: Optional[int] = None
    last_fail_at: Optional[int] = None
    last_fail_reason: Optional[str] = None
    last_sync_at: Optional[int] = None
    tags: Optional[list[str]] = None
    add_tags: list[str] = Field(default_factory=list)
    remove_tags: list[str] = Field(default_factory=list)
    note: Optional[str] = None
    last_asset_clear_at: Optional[int] = None
    metadata_merge: dict[str, Any] = Field(default_factory=dict)
    clear_failures: bool = False
    restore: bool = False

    @field_validator("add_tags", "remove_tags", mode="before")
    @classmethod
    def normalize_tags(cls, value: Any) -> list[str]:
        if not value:
            return []
        if isinstance(value, str):
            value = [part.strip() for part in value.split(",")]
        return [str(item).strip() for item in value if str(item).strip()]

    def apply(self, record: AccountRecord, *, revision: Optional[int] = None) -> AccountRecord:
        updated = record.model_copy(deep=True)
        revision = now_ms() if revision is None else revision

        if self.pool_name is not None:
            updated.pool_name = self.pool_name
        if self.status is not None:
            updated.status = self.status
        if self.quota is not None:
            updated.quota = self.quota
        if self.consumed is not None:
            updated.consumed = self.consumed
        if self.last_used_at is not None:
            updated.last_used_at = self.last_used_at
        if self.use_count is not None:
            updated.use_count = self.use_count
        if self.fail_count is not None:
            updated.fail_count = self.fail_count
        if self.last_fail_at is not None:
            updated.last_fail_at = self.last_fail_at
        if self.last_fail_reason is not None:
            updated.last_fail_reason = self.last_fail_reason
        if self.last_sync_at is not None:
            updated.last_sync_at = self.last_sync_at
        if self.tags is not None:
            updated.tags = list(self.tags)
        if self.add_tags:
            for tag in self.add_tags:
                if tag not in updated.tags:
                    updated.tags.append(tag)
        if self.remove_tags:
            updated.tags = [tag for tag in updated.tags if tag not in set(self.remove_tags)]
        if self.note is not None:
            updated.note = self.note
        if self.last_asset_clear_at is not None:
            updated.last_asset_clear_at = self.last_asset_clear_at
        if self.metadata_merge:
            updated.metadata.update(self.metadata_merge)
        if self.clear_failures:
            updated.fail_count = 0
            updated.last_fail_at = None
            updated.last_fail_reason = None
        if self.restore:
            updated.deleted_at = None
        updated.updated_at = revision
        return updated


class BulkReplacePoolCommand(BaseModel):
    pool_name: str
    items: list[AccountUpsert] = Field(default_factory=list)
