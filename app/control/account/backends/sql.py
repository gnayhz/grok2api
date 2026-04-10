"""Shared SQLAlchemy-based backend for MySQL and PostgreSQL.

Both dialects share the same table schema and query logic;
only the DDL fragments and upsert syntax differ.
"""

import json
from typing import Any
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

import sqlalchemy as sa
from sqlalchemy.ext.asyncio import AsyncEngine, async_sessionmaker, create_async_engine

from app.platform.runtime.clock import now_ms
from ..commands import AccountPatch, AccountUpsert, BulkReplacePoolCommand, ListAccountsQuery
from ..enums import AccountStatus
from ..models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountPage,
    AccountRecord,
    RuntimeSnapshot,
)
from ..quota_defaults import default_quota_set

_TBL_ACCOUNTS = "accounts"
_TBL_META     = "account_meta"

metadata = sa.MetaData()

accounts_table = sa.Table(
    _TBL_ACCOUNTS,
    metadata,
    sa.Column("token",            sa.String(512), primary_key=True),
    sa.Column("pool",             sa.Text,    nullable=False, default="basic"),
    sa.Column("status",           sa.Text,    nullable=False, default="active"),
    sa.Column("created_at",       sa.BigInteger, nullable=False),
    sa.Column("updated_at",       sa.BigInteger, nullable=False),
    sa.Column("tags",             sa.Text,    nullable=False, default="[]"),
    sa.Column("quota_auto",       sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_fast",       sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_expert",     sa.Text,    nullable=False, default="{}"),
    sa.Column("quota_heavy",      sa.Text,    nullable=False, default="{}"),
    sa.Column("usage_use_count",  sa.Integer, nullable=False, default=0),
    sa.Column("usage_fail_count", sa.Integer, nullable=False, default=0),
    sa.Column("usage_sync_count", sa.Integer, nullable=False, default=0),
    sa.Column("last_use_at",      sa.BigInteger),
    sa.Column("last_fail_at",     sa.BigInteger),
    sa.Column("last_fail_reason", sa.Text),
    sa.Column("last_sync_at",     sa.BigInteger),
    sa.Column("last_clear_at",    sa.BigInteger),
    sa.Column("state_reason",     sa.Text),
    sa.Column("deleted_at",       sa.BigInteger),
    sa.Column("ext",              sa.Text,    nullable=False, default="{}"),
    sa.Column("revision",         sa.BigInteger, nullable=False, default=0),
)

meta_table = sa.Table(
    _TBL_META,
    metadata,
    sa.Column("key",   sa.String(128), primary_key=True),
    sa.Column("value", sa.Text, nullable=False),
)

_SQL_SSL_PARAM_KEYS = ("sslmode", "ssl-mode", "ssl")

_PG_SSL_MODE_ALIASES: dict[str, str] = {
    "disable": "disable",
    "disabled": "disable",
    "false": "disable",
    "0": "disable",
    "no": "disable",
    "off": "disable",
    "prefer": "prefer",
    "preferred": "prefer",
    "allow": "allow",
    "require": "require",
    "required": "require",
    "true": "require",
    "1": "require",
    "yes": "require",
    "on": "require",
    "verify-ca": "verify-ca",
    "verify_ca": "verify-ca",
    "verify-full": "verify-full",
    "verify_full": "verify-full",
    "verify-identity": "verify-full",
    "verify_identity": "verify-full",
}

_MYSQL_SSL_MODE_ALIASES: dict[str, str] = {
    "disable": "disabled",
    "disabled": "disabled",
    "false": "disabled",
    "0": "disabled",
    "no": "disabled",
    "off": "disabled",
    "prefer": "preferred",
    "preferred": "preferred",
    "allow": "preferred",
    "require": "required",
    "required": "required",
    "true": "required",
    "1": "required",
    "yes": "required",
    "on": "required",
    "verify-ca": "verify_ca",
    "verify_ca": "verify_ca",
    "verify-full": "verify_identity",
    "verify_full": "verify_identity",
    "verify-identity": "verify_identity",
    "verify_identity": "verify_identity",
}


def _normalize_sql_url(dialect: str, url: str) -> str:
    """Rewrite SQL URLs to the async SQLAlchemy dialect form."""
    if not url or "://" not in url:
        return url
    if dialect == "mysql":
        if url.startswith("mysql://"):
            return f"mysql+aiomysql://{url[len('mysql://') :]}"
        if url.startswith("mariadb://"):
            return f"mysql+aiomysql://{url[len('mariadb://') :]}"
        if url.startswith("mariadb+aiomysql://"):
            return f"mysql+aiomysql://{url[len('mariadb+aiomysql://') :]}"
        return url
    if url.startswith("postgres://"):
        return f"postgresql+asyncpg://{url[len('postgres://') :]}"
    if url.startswith("postgresql://"):
        return f"postgresql+asyncpg://{url[len('postgresql://') :]}"
    if url.startswith("pgsql://"):
        return f"postgresql+asyncpg://{url[len('pgsql://') :]}"
    return url


def _normalize_ssl_mode(dialect: str, raw_mode: str) -> str:
    if not raw_mode:
        raise ValueError("SSL mode cannot be empty")

    mode = raw_mode.strip().lower().replace(" ", "")
    if dialect == "mysql":
        canonical = _MYSQL_SSL_MODE_ALIASES.get(mode)
    else:
        canonical = _PG_SSL_MODE_ALIASES.get(mode)
    if not canonical:
        raise ValueError(f"Unsupported SSL mode {raw_mode!r} for SQL dialect {dialect!r}")
    return canonical


def _build_mysql_ssl_context(mode: str):
    import ssl

    if mode == "disabled":
        return None

    ctx = ssl.create_default_context()
    if mode in ("preferred", "required"):
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    elif mode == "verify_ca":
        ctx.check_hostname = False
    return ctx


def _build_sql_connect_args(dialect: str, raw_ssl_mode: str | None) -> dict[str, Any] | None:
    if not raw_ssl_mode:
        return None

    mode = _normalize_ssl_mode(dialect, raw_ssl_mode)
    if dialect == "mysql":
        ctx = _build_mysql_ssl_context(mode)
        return {"ssl": ctx} if ctx is not None else None
    return {"ssl": mode}


def _prepare_sql_url_and_connect_args(
    dialect: str,
    url: str,
) -> tuple[str, dict[str, Any] | None]:
    """Strip SSL query params from the URL and translate them to connect_args."""
    normalized_url = _normalize_sql_url(dialect, url)
    if "://" not in normalized_url:
        return normalized_url, None

    parsed = urlparse(normalized_url)
    ssl_mode: str | None = None
    filtered_query_items: list[tuple[str, str]] = []
    ssl_param_keys = {key.lower() for key in _SQL_SSL_PARAM_KEYS}
    for key, value in parse_qsl(parsed.query, keep_blank_values=True):
        if key.lower() in ssl_param_keys:
            if ssl_mode is None and value:
                ssl_mode = value
            continue
        filtered_query_items.append((key, value))

    cleaned_url = urlunparse(
        parsed._replace(query=urlencode(filtered_query_items, doseq=True))
    )
    return cleaned_url, _build_sql_connect_args(dialect, ssl_mode)


def _row_to_record(row: Any) -> AccountRecord:
    d = dict(row._mapping)
    d["tags"]  = json.loads(d.get("tags")  or "[]")
    heavy_raw  = d.pop("quota_heavy", "{}") or "{}"
    heavy_dict = json.loads(heavy_raw)
    d["quota"] = {
        "auto":   json.loads(d.pop("quota_auto",   "{}") or "{}"),
        "fast":   json.loads(d.pop("quota_fast",   "{}") or "{}"),
        "expert": json.loads(d.pop("quota_expert", "{}") or "{}"),
        **({"heavy": heavy_dict} if heavy_dict else {}),
    }
    d["ext"] = json.loads(d.get("ext") or "{}")
    return AccountRecord.model_validate(d)


class SqlAccountRepository:
    """Async SQLAlchemy-based repository for MySQL / PostgreSQL."""

    def __init__(self, engine: AsyncEngine, *, dialect: str = "mysql") -> None:
        self._engine  = engine
        self._dialect = dialect   # "mysql" | "postgresql"
        self._session = async_sessionmaker(engine, expire_on_commit=False)

    # ------------------------------------------------------------------
    # Revision helpers (run inside a transaction)
    # ------------------------------------------------------------------

    async def _bump_revision(self, conn: Any) -> int:
        await conn.execute(
            meta_table.update()
            .where(meta_table.c.key == "revision")
            .values(value=sa.cast(
                sa.cast(meta_table.c.value, sa.BigInteger) + 1, sa.Text
            ))
        )
        row = await conn.execute(
            sa.select(meta_table.c.value).where(meta_table.c.key == "revision")
        )
        return int(row.scalar())

    async def _get_revision(self, conn: Any) -> int:
        row = await conn.execute(
            sa.select(meta_table.c.value).where(meta_table.c.key == "revision")
        )
        v = row.scalar()
        return int(v) if v else 0

    # ------------------------------------------------------------------
    # Upsert — dialect-specific
    # ------------------------------------------------------------------

    def _build_upsert(self, row: dict[str, Any]):
        if self._dialect == "postgresql":
            from sqlalchemy.dialects.postgresql import insert
            stmt = insert(accounts_table).values(**row)
            # On conflict, update all columns except token and created_at.
            update_cols = {k: stmt.excluded[k] for k in row if k not in ("token", "created_at")}
            return stmt.on_conflict_do_update(index_elements=["token"], set_=update_cols)
        else:
            # MySQL
            from sqlalchemy.dialects.mysql import insert
            stmt = insert(accounts_table).values(**row)
            update_cols = {k: stmt.inserted[k] for k in row if k not in ("token", "created_at")}
            return stmt.on_duplicate_key_update(**update_cols)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def initialize(self) -> None:
        async with self._engine.begin() as conn:
            await conn.run_sync(metadata.create_all)
            # Seed revision row.
            if self._dialect == "postgresql":
                from sqlalchemy.dialects.postgresql import insert as pg_insert
                await conn.execute(
                    pg_insert(meta_table)
                    .values(key="revision", value="0")
                    .on_conflict_do_nothing()
                )
            else:
                from sqlalchemy.dialects.mysql import insert as my_insert
                await conn.execute(
                    my_insert(meta_table)
                    .values(key="revision", value="0")
                    .on_duplicate_key_update(value="0")
                )

    async def get_revision(self) -> int:
        async with self._engine.connect() as conn:
            return await self._get_revision(conn)

    async def runtime_snapshot(self) -> RuntimeSnapshot:
        async with self._engine.connect() as conn:
            rev = await self._get_revision(conn)
            rows = (await conn.execute(
                sa.select(accounts_table).where(accounts_table.c.deleted_at.is_(None))
            )).fetchall()
            return RuntimeSnapshot(revision=rev, items=[_row_to_record(r) for r in rows])

    async def scan_changes(
        self,
        since_revision: int,
        *,
        limit: int = 5000,
    ) -> AccountChangeSet:
        async with self._engine.connect() as conn:
            rev = await self._get_revision(conn)
            rows = (await conn.execute(
                sa.select(accounts_table)
                .where(accounts_table.c.revision > since_revision)
                .order_by(accounts_table.c.revision)
                .limit(limit)
            )).fetchall()
            items: list[AccountRecord] = []
            deleted: list[str] = []
            for row in rows:
                r = _row_to_record(row)
                if r.is_deleted():
                    deleted.append(r.token)
                else:
                    items.append(r)
            return AccountChangeSet(
                revision=rev,
                items=items,
                deleted_tokens=deleted,
                has_more=len(rows) == limit,
            )

    async def upsert_accounts(
        self,
        items: list[AccountUpsert],
    ) -> AccountMutationResult:
        if not items:
            return AccountMutationResult()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            count = 0
            for item in items:
                try:
                    token = AccountRecord.model_validate({"token": item.token, "pool": item.pool}).token
                except Exception:
                    continue
                pool = item.pool if item.pool in ("basic", "super", "heavy") else "basic"
                qs   = default_quota_set(pool)
                row  = {
                    "token":            token,
                    "pool":             pool,
                    "status":           "active",
                    "created_at":       ts,
                    "updated_at":       ts,
                    "deleted_at":       None,   # clear soft-delete on re-import
                    "tags":             json.dumps(item.tags),
                    "quota_auto":       json.dumps(qs.auto.to_dict()),
                    "quota_fast":       json.dumps(qs.fast.to_dict()),
                    "quota_expert":     json.dumps(qs.expert.to_dict()),
                    "quota_heavy":      json.dumps(qs.heavy.to_dict()) if qs.heavy else "{}",
                    "usage_use_count":  0,
                    "usage_fail_count": 0,
                    "usage_sync_count": 0,
                    "ext":              json.dumps(item.ext),
                    "revision":         rev,
                }
                await conn.execute(self._build_upsert(row))
                count += 1
            return AccountMutationResult(upserted=count, revision=rev)

    async def patch_accounts(
        self,
        patches: list[AccountPatch],
    ) -> AccountMutationResult:
        if not patches:
            return AccountMutationResult()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            count = 0
            for patch in patches:
                row = (await conn.execute(
                    sa.select(accounts_table).where(accounts_table.c.token == patch.token)
                )).fetchone()
                if row is None:
                    continue
                record = _row_to_record(row)

                updates: dict[str, Any] = {"updated_at": ts, "revision": rev}
                if patch.pool is not None:
                    updates["pool"] = patch.pool
                if patch.status is not None:
                    updates["status"] = patch.status.value
                if patch.state_reason is not None:
                    updates["state_reason"] = patch.state_reason
                if patch.last_use_at is not None:
                    updates["last_use_at"] = patch.last_use_at
                if patch.last_fail_at is not None:
                    updates["last_fail_at"] = patch.last_fail_at
                if patch.last_fail_reason is not None:
                    updates["last_fail_reason"] = patch.last_fail_reason
                if patch.last_sync_at is not None:
                    updates["last_sync_at"] = patch.last_sync_at
                if patch.last_clear_at is not None:
                    updates["last_clear_at"] = patch.last_clear_at
                if patch.quota_auto is not None:
                    updates["quota_auto"] = json.dumps(patch.quota_auto)
                if patch.quota_fast is not None:
                    updates["quota_fast"] = json.dumps(patch.quota_fast)
                if patch.quota_expert is not None:
                    updates["quota_expert"] = json.dumps(patch.quota_expert)
                if patch.quota_heavy is not None:
                    updates["quota_heavy"] = json.dumps(patch.quota_heavy)
                if patch.usage_use_delta is not None:
                    updates["usage_use_count"] = max(0, record.usage_use_count + patch.usage_use_delta)
                if patch.usage_fail_delta is not None:
                    updates["usage_fail_count"] = max(0, record.usage_fail_count + patch.usage_fail_delta)
                if patch.usage_sync_delta is not None:
                    updates["usage_sync_count"] = max(0, record.usage_sync_count + patch.usage_sync_delta)

                tags = list(record.tags)
                if patch.tags is not None:
                    tags = patch.tags
                if patch.add_tags:
                    for t in patch.add_tags:
                        if t not in tags:
                            tags.append(t)
                if patch.remove_tags:
                    tags = [t for t in tags if t not in patch.remove_tags]
                updates["tags"] = json.dumps(tags)

                ext = dict(record.ext)
                if patch.ext_merge:
                    ext.update(patch.ext_merge)
                if patch.clear_failures:
                    for k in ("cooldown_until", "cooldown_reason", "disabled_at",
                              "disabled_reason", "expired_at", "expired_reason",
                              "forbidden_strikes"):
                        ext.pop(k, None)
                    updates["status"]           = AccountStatus.ACTIVE.value
                    updates["usage_fail_count"] = 0
                    updates["last_fail_at"]     = None
                    updates["last_fail_reason"] = None
                    updates["state_reason"]     = None
                updates["ext"] = json.dumps(ext)

                await conn.execute(
                    accounts_table.update()
                    .where(accounts_table.c.token == patch.token)
                    .values(**updates)
                )
                count += 1
            return AccountMutationResult(patched=count, revision=rev)

    async def delete_accounts(
        self,
        tokens: list[str],
    ) -> AccountMutationResult:
        if not tokens:
            return AccountMutationResult()
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            result = await conn.execute(
                accounts_table.update()
                .where(
                    accounts_table.c.token.in_(tokens),
                    accounts_table.c.deleted_at.is_(None),
                )
                .values(deleted_at=ts, updated_at=ts, revision=rev)
            )
            return AccountMutationResult(deleted=result.rowcount, revision=rev)

    async def get_accounts(
        self,
        tokens: list[str],
    ) -> list[AccountRecord]:
        if not tokens:
            return []
        async with self._engine.connect() as conn:
            rows = (await conn.execute(
                sa.select(accounts_table).where(accounts_table.c.token.in_(tokens))
            )).fetchall()
            return [_row_to_record(r) for r in rows]

    async def list_accounts(
        self,
        query: ListAccountsQuery,
    ) -> AccountPage:
        async with self._engine.connect() as conn:
            stmt = sa.select(accounts_table)
            if not query.include_deleted:
                stmt = stmt.where(accounts_table.c.deleted_at.is_(None))
            if query.pool:
                stmt = stmt.where(accounts_table.c.pool == query.pool)
            if query.status:
                stmt = stmt.where(accounts_table.c.status == query.status.value)

            total_row = (await conn.execute(
                sa.select(sa.func.count()).select_from(stmt.subquery())
            )).scalar()
            total = int(total_row or 0)

            sort_col = getattr(accounts_table.c, query.sort_by, accounts_table.c.updated_at)
            if query.sort_desc:
                stmt = stmt.order_by(sort_col.desc())
            else:
                stmt = stmt.order_by(sort_col.asc())
            offset = (query.page - 1) * query.page_size
            stmt = stmt.limit(query.page_size).offset(offset)

            rows = (await conn.execute(stmt)).fetchall()
            rev  = await self._get_revision(conn)
            return AccountPage(
                items=[_row_to_record(r) for r in rows],
                total=total,
                page=query.page,
                page_size=query.page_size,
                total_pages=max(1, (total + query.page_size - 1) // query.page_size),
                revision=rev,
            )

    async def replace_pool(
        self,
        command: BulkReplacePoolCommand,
    ) -> AccountMutationResult:
        async with self._engine.begin() as conn:
            rev = await self._bump_revision(conn)
            ts  = now_ms()
            del_result = await conn.execute(
                accounts_table.update()
                .where(
                    accounts_table.c.pool == command.pool,
                    accounts_table.c.deleted_at.is_(None),
                )
                .values(deleted_at=ts, updated_at=ts, revision=rev)
            )
            deleted = del_result.rowcount

        upserted_result = await self.upsert_accounts(command.upserts)
        return AccountMutationResult(
            upserted=upserted_result.upserted,
            deleted=deleted,
            revision=upserted_result.revision,
        )

    async def close(self) -> None:
        """Dispose the SQLAlchemy connection pool."""
        await self._engine.dispose()


def create_mysql_engine(url: str) -> AsyncEngine:
    """Create an async SQLAlchemy engine for MySQL."""
    normalized_url, connect_args = _prepare_sql_url_and_connect_args("mysql", url)
    return create_async_engine(
        normalized_url,
        pool_size=10,
        max_overflow=20,
        pool_pre_ping=True,
        **({"connect_args": connect_args} if connect_args else {}),
    )


def create_pgsql_engine(url: str) -> AsyncEngine:
    """Create an async SQLAlchemy engine for PostgreSQL."""
    normalized_url, connect_args = _prepare_sql_url_and_connect_args("postgresql", url)
    return create_async_engine(
        normalized_url,
        pool_size=10,
        max_overflow=20,
        pool_pre_ping=True,
        **({"connect_args": connect_args} if connect_args else {}),
    )


__all__ = ["SqlAccountRepository", "create_mysql_engine", "create_pgsql_engine"]
