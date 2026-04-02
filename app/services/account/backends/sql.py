"""
MySQL/PostgreSQL repository for account management.
"""

from __future__ import annotations

from typing import Any, Sequence

from sqlalchemy import bindparam, text
from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine

from app.services.account.codec import (
    build_page,
    compute_revision,
    decode_json,
    encode_json,
)
from app.services.account.commands import AccountPatch, AccountUpsert, ListAccountsQuery
from app.services.account.models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountRecord,
    RuntimeSnapshot,
)
from app.services.account.repository import AccountRepository
from app.services.account.storage_layout import (
    ACCOUNT_SCHEMA_VERSION,
    SQL_ACCOUNT_META_TABLE,
    SQL_ACCOUNT_RECORDS_TABLE,
)


class SQLAccountRepository(AccountRepository):
    def __init__(self, url: str):
        self.url = url
        self.dialect = url.split(":", 1)[0].split("+", 1)[0].lower()
        self.engine = create_async_engine(
            url,
            echo=False,
            pool_size=20,
            max_overflow=10,
            pool_recycle=3600,
            pool_pre_ping=True,
        )
        self.async_session = async_sessionmaker(self.engine, expire_on_commit=False)
        self._initialized = False
        self._records_table = SQL_ACCOUNT_RECORDS_TABLE
        self._meta_table = SQL_ACCOUNT_META_TABLE

    async def initialize(self) -> None:
        if self._initialized:
            return
        async with self.engine.begin() as conn:
            await conn.execute(
                text(
                    f"""
                    CREATE TABLE IF NOT EXISTS {self._records_table} (
                        token VARCHAR(512) PRIMARY KEY,
                        pool_name VARCHAR(64) NOT NULL,
                        status VARCHAR(16) NOT NULL,
                        quota INT NOT NULL,
                        consumed INT NOT NULL,
                        created_at BIGINT NOT NULL,
                        updated_at BIGINT NOT NULL,
                        last_used_at BIGINT NULL,
                        use_count INT NOT NULL,
                        fail_count INT NOT NULL,
                        last_fail_at BIGINT NULL,
                        last_fail_reason TEXT NULL,
                        last_sync_at BIGINT NULL,
                        tags_json TEXT NOT NULL,
                        note TEXT NOT NULL,
                        last_asset_clear_at BIGINT NULL,
                        metadata_json TEXT NOT NULL,
                        deleted_at BIGINT NULL
                    )
                    """
                )
            )
            await conn.execute(
                text(
                    f"""
                    CREATE TABLE IF NOT EXISTS {self._meta_table} (
                        key_name VARCHAR(128) PRIMARY KEY,
                        value TEXT NOT NULL
                    )
                    """
                )
            )
            for stmt in (
                f"CREATE INDEX IF NOT EXISTS idx_account_pool ON {self._records_table} (pool_name)",
                f"CREATE INDEX IF NOT EXISTS idx_account_status ON {self._records_table} (status)",
                f"CREATE INDEX IF NOT EXISTS idx_account_updated ON {self._records_table} (updated_at)",
                f"CREATE INDEX IF NOT EXISTS idx_account_deleted ON {self._records_table} (deleted_at)",
            ):
                try:
                    await conn.execute(text(stmt))
                except Exception:
                    pass
            if self.dialect in ("mysql", "mariadb"):
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES (:key_name, :value)
                    ON DUPLICATE KEY UPDATE value=VALUES(value)
                    """
                )
            else:
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES (:key_name, :value)
                    ON CONFLICT (key_name) DO UPDATE SET value=EXCLUDED.value
                    """
                )
            await conn.execute(
                meta_stmt,
                [
                    {"key_name": "schema_version", "value": ACCOUNT_SCHEMA_VERSION},
                    {"key_name": "revision", "value": "0"},
                ],
            )
        self._initialized = True

    async def close(self) -> None:
        await self.engine.dispose()

    def _row_to_record(self, row: Any) -> AccountRecord:
        data = dict(row._mapping)
        return AccountRecord(
            token=data["token"],
            pool_name=data["pool_name"],
            status=data["status"],
            quota=data["quota"],
            consumed=data["consumed"],
            created_at=data["created_at"],
            updated_at=data["updated_at"],
            last_used_at=data.get("last_used_at"),
            use_count=data["use_count"],
            fail_count=data["fail_count"],
            last_fail_at=data.get("last_fail_at"),
            last_fail_reason=data.get("last_fail_reason"),
            last_sync_at=data.get("last_sync_at"),
            tags=decode_json(data.get("tags_json"), []),
            note=data.get("note") or "",
            last_asset_clear_at=data.get("last_asset_clear_at"),
            metadata=decode_json(data.get("metadata_json"), {}),
            deleted_at=data.get("deleted_at"),
        )

    def _record_params(self, record: AccountRecord) -> dict[str, Any]:
        return {
            "token": record.token,
            "pool_name": record.pool_name,
            "status": record.status.value,
            "quota": record.quota,
            "consumed": record.consumed,
            "created_at": record.created_at,
            "updated_at": record.updated_at,
            "last_used_at": record.last_used_at,
            "use_count": record.use_count,
            "fail_count": record.fail_count,
            "last_fail_at": record.last_fail_at,
            "last_fail_reason": record.last_fail_reason,
            "last_sync_at": record.last_sync_at,
            "tags_json": encode_json(record.tags),
            "note": record.note,
            "last_asset_clear_at": record.last_asset_clear_at,
            "metadata_json": encode_json(record.metadata),
            "deleted_at": record.deleted_at,
        }

    async def get_revision(self) -> int:
        await self.initialize()
        async with self.async_session() as session:
            row = (
                await session.execute(
                    text(
                        f"SELECT value FROM {self._meta_table} WHERE key_name = 'revision'"
                    )
                )
            ).first()
            if row and row[0]:
                return int(row[0])
            row = (
                await session.execute(
                    text(
                        f"SELECT COALESCE(MAX(updated_at), 0) AS revision FROM {self._records_table}"
                    )
                )
            ).one()
            return int(row[0] or 0)

    async def get_metadata(self) -> dict[str, str]:
        await self.initialize()
        async with self.async_session() as session:
            rows = (
                await session.execute(
                    text(f"SELECT key_name, value FROM {self._meta_table}")
                )
            ).fetchall()
        return {row[0]: row[1] for row in rows}

    async def set_metadata(self, mapping: dict[str, str]) -> None:
        await self.initialize()
        if not mapping:
            return
        if self.dialect in ("mysql", "mariadb"):
            stmt = text(
                f"""
                INSERT INTO {self._meta_table}(key_name, value)
                VALUES (:key_name, :value)
                ON DUPLICATE KEY UPDATE value=VALUES(value)
                """
            )
        else:
            stmt = text(
                f"""
                INSERT INTO {self._meta_table}(key_name, value)
                VALUES (:key_name, :value)
                ON CONFLICT (key_name) DO UPDATE SET value=EXCLUDED.value
                """
            )
        async with self.async_session() as session:
            await session.execute(
                stmt,
                [{"key_name": key, "value": value} for key, value in mapping.items()],
            )
            await session.commit()

    async def get_accounts(self, tokens: Sequence[str]) -> dict[str, AccountRecord]:
        await self.initialize()
        token_list = [token.replace("sso=", "") for token in tokens]
        if not token_list:
            return {}
        async with self.async_session() as session:
            stmt = text(
                f"SELECT * FROM {self._records_table} WHERE token IN :tokens"
            ).bindparams(bindparam("tokens", expanding=True))
            rows = (await session.execute(stmt, {"tokens": token_list})).fetchall()
            return {row[0]: self._row_to_record(row) for row in rows}

    async def list_accounts(self, query: ListAccountsQuery):
        await self.initialize()
        async with self.async_session() as session:
            rows = (
                await session.execute(text(f"SELECT * FROM {self._records_table}"))
            ).fetchall()
        records = [self._row_to_record(row) for row in rows]
        revision = max((record.updated_at for record in records), default=await self.get_revision())
        return build_page(records, query=query, revision=revision)

    async def upsert_accounts(
        self, items: Sequence[AccountUpsert]
    ) -> AccountMutationResult:
        await self.initialize()
        if not items:
            return AccountMutationResult(revision=await self.get_revision())
        existing = await self.get_accounts([item.token for item in items])
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        records = [
            item.to_record(
                current=existing.get(item.token.replace("sso=", "")),
                revision=revision,
            )
            for item in items
        ]
        await self._execute_upsert_records(records)
        return AccountMutationResult(upserted=len(records), revision=revision)

    async def _execute_upsert_records(self, records: Sequence[AccountRecord]) -> None:
        if not records:
            return
        payloads = [self._record_params(record) for record in records]
        async with self.async_session() as session:
            if self.dialect in ("mysql", "mariadb"):
                stmt = text(
                    f"""
                    INSERT INTO {self._records_table} (
                        token, pool_name, status, quota, consumed, created_at, updated_at,
                        last_used_at, use_count, fail_count, last_fail_at, last_fail_reason,
                        last_sync_at, tags_json, note, last_asset_clear_at, metadata_json, deleted_at
                    ) VALUES (
                        :token, :pool_name, :status, :quota, :consumed, :created_at, :updated_at,
                        :last_used_at, :use_count, :fail_count, :last_fail_at, :last_fail_reason,
                        :last_sync_at, :tags_json, :note, :last_asset_clear_at, :metadata_json, :deleted_at
                    )
                    ON DUPLICATE KEY UPDATE
                        pool_name=VALUES(pool_name),
                        status=VALUES(status),
                        quota=VALUES(quota),
                        consumed=VALUES(consumed),
                        created_at=VALUES(created_at),
                        updated_at=VALUES(updated_at),
                        last_used_at=VALUES(last_used_at),
                        use_count=VALUES(use_count),
                        fail_count=VALUES(fail_count),
                        last_fail_at=VALUES(last_fail_at),
                        last_fail_reason=VALUES(last_fail_reason),
                        last_sync_at=VALUES(last_sync_at),
                        tags_json=VALUES(tags_json),
                        note=VALUES(note),
                        last_asset_clear_at=VALUES(last_asset_clear_at),
                        metadata_json=VALUES(metadata_json),
                        deleted_at=VALUES(deleted_at)
                    """
                )
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES ('revision', :revision)
                    ON DUPLICATE KEY UPDATE value=VALUES(value)
                    """
                )
            else:
                stmt = text(
                    f"""
                    INSERT INTO {self._records_table} (
                        token, pool_name, status, quota, consumed, created_at, updated_at,
                        last_used_at, use_count, fail_count, last_fail_at, last_fail_reason,
                        last_sync_at, tags_json, note, last_asset_clear_at, metadata_json, deleted_at
                    ) VALUES (
                        :token, :pool_name, :status, :quota, :consumed, :created_at, :updated_at,
                        :last_used_at, :use_count, :fail_count, :last_fail_at, :last_fail_reason,
                        :last_sync_at, :tags_json, :note, :last_asset_clear_at, :metadata_json, :deleted_at
                    )
                    ON CONFLICT (token) DO UPDATE SET
                        pool_name=EXCLUDED.pool_name,
                        status=EXCLUDED.status,
                        quota=EXCLUDED.quota,
                        consumed=EXCLUDED.consumed,
                        created_at=EXCLUDED.created_at,
                        updated_at=EXCLUDED.updated_at,
                        last_used_at=EXCLUDED.last_used_at,
                        use_count=EXCLUDED.use_count,
                        fail_count=EXCLUDED.fail_count,
                        last_fail_at=EXCLUDED.last_fail_at,
                        last_fail_reason=EXCLUDED.last_fail_reason,
                        last_sync_at=EXCLUDED.last_sync_at,
                        tags_json=EXCLUDED.tags_json,
                        note=EXCLUDED.note,
                        last_asset_clear_at=EXCLUDED.last_asset_clear_at,
                        metadata_json=EXCLUDED.metadata_json,
                        deleted_at=EXCLUDED.deleted_at
                    """
                )
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES ('revision', :revision)
                    ON CONFLICT (key_name) DO UPDATE SET value=EXCLUDED.value
                    """
                )
            await session.execute(stmt, payloads)
            await session.execute(
                meta_stmt,
                {"revision": str(max(record.updated_at for record in records))},
            )
            await session.commit()

    async def patch_accounts(
        self, patches: Sequence[AccountPatch]
    ) -> AccountMutationResult:
        await self.initialize()
        if not patches:
            return AccountMutationResult(revision=await self.get_revision())
        current = await self.get_accounts([patch.token for patch in patches])
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        records: list[AccountRecord] = []
        for patch in patches:
            token = patch.token.replace("sso=", "")
            record = current.get(token)
            if not record:
                continue
            records.append(patch.apply(record, revision=revision))
        if records:
            await self._execute_upsert_records(records)
            return AccountMutationResult(patched=len(records), revision=revision)
        return AccountMutationResult(revision=previous_revision)

    async def delete_accounts(self, tokens: Sequence[str]) -> AccountMutationResult:
        await self.initialize()
        token_list = [token.replace("sso=", "") for token in tokens]
        if not token_list:
            return AccountMutationResult(revision=await self.get_revision())
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        async with self.async_session() as session:
            stmt = text(
                f"UPDATE {self._records_table} SET deleted_at=:revision, updated_at=:revision "
                "WHERE token IN :tokens AND deleted_at IS NULL"
            ).bindparams(bindparam("tokens", expanding=True))
            res = await session.execute(
                stmt,
                {"revision": revision, "tokens": token_list},
            )
            if self.dialect in ("mysql", "mariadb"):
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES ('revision', :revision)
                    ON DUPLICATE KEY UPDATE value=VALUES(value)
                    """
                )
            else:
                meta_stmt = text(
                    f"""
                    INSERT INTO {self._meta_table}(key_name, value)
                    VALUES ('revision', :revision)
                    ON CONFLICT (key_name) DO UPDATE SET value=EXCLUDED.value
                    """
                )
            await session.execute(meta_stmt, {"revision": str(revision)})
            await session.commit()
            return AccountMutationResult(
                deleted=int(res.rowcount or 0),
                revision=revision,
            )

    async def scan_changes(
        self, since_revision: int, *, limit: int = 5000
    ) -> AccountChangeSet:
        await self.initialize()
        async with self.async_session() as session:
            rows = (
                await session.execute(
                    text(
                        f"""
                        SELECT * FROM {self._records_table}
                        WHERE updated_at > :since_revision
                        ORDER BY updated_at ASC, token ASC
                        LIMIT :limit
                        """
                    ),
                    {"since_revision": since_revision, "limit": limit + 1},
                )
            ).fetchall()
        has_more = len(rows) > limit
        rows = rows[:limit]
        records = [self._row_to_record(row) for row in rows]
        revision = max([since_revision, *[record.updated_at for record in records]])
        return AccountChangeSet(
            revision=revision,
            items=[record for record in records if record.deleted_at is None],
            deleted_tokens=[record.token for record in records if record.deleted_at is not None],
            has_more=has_more,
        )

    async def runtime_snapshot(
        self, *, include_deleted: bool = False
    ) -> RuntimeSnapshot:
        await self.initialize()
        async with self.async_session() as session:
            rows = (
                await session.execute(text(f"SELECT * FROM {self._records_table}"))
            ).fetchall()
        records = [self._row_to_record(row) for row in rows]
        if not include_deleted:
            records = [record for record in records if record.deleted_at is None]
        revision = max(
            (record.updated_at for record in records),
            default=await self.get_revision(),
        )
        return RuntimeSnapshot(revision=revision, items=records)
