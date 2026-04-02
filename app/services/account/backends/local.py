"""
Local account repository backed by SQLite.

This backend exists specifically to avoid the current full-file rewrite bottleneck.
"""

from __future__ import annotations

import asyncio
import sqlite3
from contextlib import closing
from pathlib import Path
from typing import Any, Sequence

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
from app.services.account.storage_layout import ACCOUNT_SCHEMA_VERSION


class LocalAccountRepository(AccountRepository):
    def __init__(self, db_path: Path):
        self.db_path = Path(db_path)
        self._lock = asyncio.Lock()
        self._records_table = "account_records"
        self._meta_table = "account_meta"

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute("PRAGMA busy_timeout=5000")
        return conn

    async def initialize(self) -> None:
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        await asyncio.to_thread(self._initialize_sync)

    def _initialize_sync(self) -> None:
        with closing(self._connect()) as conn:
            conn.executescript(
                f"""
                CREATE TABLE IF NOT EXISTS {self._records_table} (
                    token TEXT PRIMARY KEY,
                    pool_name TEXT NOT NULL,
                    status TEXT NOT NULL,
                    quota INTEGER NOT NULL,
                    consumed INTEGER NOT NULL,
                    created_at INTEGER NOT NULL,
                    updated_at INTEGER NOT NULL,
                    last_used_at INTEGER,
                    use_count INTEGER NOT NULL,
                    fail_count INTEGER NOT NULL,
                    last_fail_at INTEGER,
                    last_fail_reason TEXT,
                    last_sync_at INTEGER,
                    tags_json TEXT NOT NULL,
                    note TEXT NOT NULL,
                    last_asset_clear_at INTEGER,
                    metadata_json TEXT NOT NULL,
                    deleted_at INTEGER
                );
                CREATE TABLE IF NOT EXISTS {self._meta_table} (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_account_pool ON {self._records_table}(pool_name);
                CREATE INDEX IF NOT EXISTS idx_account_status ON {self._records_table}(status);
                CREATE INDEX IF NOT EXISTS idx_account_updated ON {self._records_table}(updated_at);
                CREATE INDEX IF NOT EXISTS idx_account_deleted ON {self._records_table}(deleted_at);
                """
            )
            conn.execute(
                f"INSERT OR IGNORE INTO {self._meta_table}(key, value) VALUES ('schema_version', ?)",
                [ACCOUNT_SCHEMA_VERSION],
            )
            conn.execute(
                f"INSERT OR IGNORE INTO {self._meta_table}(key, value) VALUES ('revision', '0')"
            )
            conn.commit()

    def _row_to_record(self, row: sqlite3.Row) -> AccountRecord:
        return AccountRecord(
            token=row["token"],
            pool_name=row["pool_name"],
            status=row["status"],
            quota=row["quota"],
            consumed=row["consumed"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
            last_used_at=row["last_used_at"],
            use_count=row["use_count"],
            fail_count=row["fail_count"],
            last_fail_at=row["last_fail_at"],
            last_fail_reason=row["last_fail_reason"],
            last_sync_at=row["last_sync_at"],
            tags=decode_json(row["tags_json"], []),
            note=row["note"] or "",
            last_asset_clear_at=row["last_asset_clear_at"],
            metadata=decode_json(row["metadata_json"], {}),
            deleted_at=row["deleted_at"],
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

    async def close(self) -> None:
        return None

    async def get_revision(self) -> int:
        return await asyncio.to_thread(self._get_revision_sync)

    def _get_revision_sync(self) -> int:
        with closing(self._connect()) as conn:
            row = conn.execute(
                f"SELECT value FROM {self._meta_table} WHERE key = 'revision'"
            ).fetchone()
            if row and row["value"]:
                return int(row["value"])
            row = conn.execute(
                f"SELECT COALESCE(MAX(updated_at), 0) AS revision FROM {self._records_table}"
            ).fetchone()
            return int(row["revision"] if row else 0)

    async def get_metadata(self) -> dict[str, str]:
        return await asyncio.to_thread(self._get_metadata_sync)

    def _get_metadata_sync(self) -> dict[str, str]:
        with closing(self._connect()) as conn:
            rows = conn.execute(f"SELECT key, value FROM {self._meta_table}").fetchall()
        return {row["key"]: row["value"] for row in rows}

    async def set_metadata(self, mapping: dict[str, str]) -> None:
        if not mapping:
            return
        await asyncio.to_thread(self._set_metadata_sync, mapping)

    def _set_metadata_sync(self, mapping: dict[str, str]) -> None:
        with closing(self._connect()) as conn:
            conn.executemany(
                f"INSERT INTO {self._meta_table}(key, value) VALUES (?, ?) "
                f"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                list(mapping.items()),
            )
            conn.commit()

    async def get_accounts(self, tokens: Sequence[str]) -> dict[str, AccountRecord]:
        if not tokens:
            return {}
        token_list = [token.replace("sso=", "") for token in tokens]
        return await asyncio.to_thread(self._get_accounts_sync, token_list)

    def _get_accounts_sync(self, tokens: list[str]) -> dict[str, AccountRecord]:
        placeholders = ",".join("?" for _ in tokens)
        with closing(self._connect()) as conn:
            rows = conn.execute(
                f"SELECT * FROM {self._records_table} WHERE token IN ({placeholders})",
                tokens,
            ).fetchall()
        return {row["token"]: self._row_to_record(row) for row in rows}

    async def list_accounts(self, query: ListAccountsQuery):
        rows = await asyncio.to_thread(self._list_all_sync)
        revision = max((row.updated_at for row in rows), default=await self.get_revision())
        return build_page(rows, query=query, revision=revision)

    def _list_all_sync(self) -> list[AccountRecord]:
        with closing(self._connect()) as conn:
            rows = conn.execute(f"SELECT * FROM {self._records_table}").fetchall()
        return [self._row_to_record(row) for row in rows]

    async def upsert_accounts(
        self, items: Sequence[AccountUpsert]
    ) -> AccountMutationResult:
        if not items:
            return AccountMutationResult(revision=await self.get_revision())
        async with self._lock:
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
            await asyncio.to_thread(self._upsert_sync, records)
            return AccountMutationResult(upserted=len(records), revision=revision)

    def _upsert_sync(self, records: list[AccountRecord]) -> None:
        sql = f"""
            INSERT INTO {self._records_table} (
                token, pool_name, status, quota, consumed, created_at, updated_at,
                last_used_at, use_count, fail_count, last_fail_at, last_fail_reason,
                last_sync_at, tags_json, note, last_asset_clear_at, metadata_json, deleted_at
            ) VALUES (
                :token, :pool_name, :status, :quota, :consumed, :created_at, :updated_at,
                :last_used_at, :use_count, :fail_count, :last_fail_at, :last_fail_reason,
                :last_sync_at, :tags_json, :note, :last_asset_clear_at, :metadata_json, :deleted_at
            )
            ON CONFLICT(token) DO UPDATE SET
                pool_name=excluded.pool_name,
                status=excluded.status,
                quota=excluded.quota,
                consumed=excluded.consumed,
                created_at=excluded.created_at,
                updated_at=excluded.updated_at,
                last_used_at=excluded.last_used_at,
                use_count=excluded.use_count,
                fail_count=excluded.fail_count,
                last_fail_at=excluded.last_fail_at,
                last_fail_reason=excluded.last_fail_reason,
                last_sync_at=excluded.last_sync_at,
                tags_json=excluded.tags_json,
                note=excluded.note,
                last_asset_clear_at=excluded.last_asset_clear_at,
                metadata_json=excluded.metadata_json,
                deleted_at=excluded.deleted_at
        """
        with closing(self._connect()) as conn:
            conn.executemany(sql, [self._record_params(record) for record in records])
            conn.execute(
                f"INSERT INTO {self._meta_table}(key, value) VALUES ('revision', ?) "
                f"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                [str(max(record.updated_at for record in records))],
            )
            conn.commit()

    async def patch_accounts(
        self, patches: Sequence[AccountPatch]
    ) -> AccountMutationResult:
        if not patches:
            return AccountMutationResult(revision=await self.get_revision())
        async with self._lock:
            current = await self.get_accounts([patch.token for patch in patches])
            previous_revision = await self.get_revision()
            revision = compute_revision(previous_revision)
            updated_records: list[AccountRecord] = []
            for patch in patches:
                token = patch.token.replace("sso=", "")
                record = current.get(token)
                if not record:
                    continue
                updated_records.append(patch.apply(record, revision=revision))
            if updated_records:
                await asyncio.to_thread(self._upsert_sync, updated_records)
            return AccountMutationResult(
                patched=len(updated_records),
                revision=revision if updated_records else previous_revision,
            )

    async def delete_accounts(self, tokens: Sequence[str]) -> AccountMutationResult:
        if not tokens:
            return AccountMutationResult(revision=await self.get_revision())
        async with self._lock:
            previous_revision = await self.get_revision()
            revision = compute_revision(previous_revision)
            affected = await asyncio.to_thread(self._delete_sync, list(tokens), revision)
            return AccountMutationResult(deleted=affected, revision=revision)

    def _delete_sync(self, tokens: list[str], revision: int) -> int:
        token_list = [token.replace("sso=", "") for token in tokens]
        placeholders = ",".join("?" for _ in token_list)
        with closing(self._connect()) as conn:
            cur = conn.execute(
                f"""
                UPDATE {self._records_table}
                SET deleted_at = ?, updated_at = ?
                WHERE token IN ({placeholders}) AND deleted_at IS NULL
                """,
                [revision, revision, *token_list],
            )
            conn.execute(
                f"INSERT INTO {self._meta_table}(key, value) VALUES ('revision', ?) "
                f"ON CONFLICT(key) DO UPDATE SET value=excluded.value",
                [str(revision)],
            )
            conn.commit()
            return int(cur.rowcount or 0)

    async def scan_changes(
        self, since_revision: int, *, limit: int = 5000
    ) -> AccountChangeSet:
        return await asyncio.to_thread(self._scan_changes_sync, since_revision, limit)

    def _scan_changes_sync(self, since_revision: int, limit: int) -> AccountChangeSet:
        with closing(self._connect()) as conn:
            rows = conn.execute(
                f"""
                SELECT * FROM {self._records_table}
                WHERE updated_at > ?
                ORDER BY updated_at ASC, token ASC
                LIMIT ?
                """,
                [since_revision, limit + 1],
            ).fetchall()
        has_more = len(rows) > limit
        rows = rows[:limit]
        items = [self._row_to_record(row) for row in rows if row["deleted_at"] is None]
        deleted_tokens = [row["token"] for row in rows if row["deleted_at"] is not None]
        revision = max([since_revision, *[row["updated_at"] for row in rows]])
        return AccountChangeSet(
            revision=revision,
            items=items,
            deleted_tokens=deleted_tokens,
            has_more=has_more,
        )

    async def runtime_snapshot(
        self, *, include_deleted: bool = False
    ) -> RuntimeSnapshot:
        rows = await self._list_all_for_runtime(include_deleted=include_deleted)
        revision = max((item.updated_at for item in rows), default=await self.get_revision())
        return RuntimeSnapshot(revision=revision, items=rows)

    async def _list_all_for_runtime(self, *, include_deleted: bool) -> list[AccountRecord]:
        records = await asyncio.to_thread(self._list_all_sync)
        if include_deleted:
            return records
        return [record for record in records if record.deleted_at is None]
