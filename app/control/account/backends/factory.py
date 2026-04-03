"""Account repository factory — selects the backend from configuration."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from app.platform.config.snapshot import get_config
from ..repository import AccountRepository


def create_repository() -> AccountRepository:
    """Instantiate the configured account storage backend.

    Config key: ``account.storage``  (default: ``"local"``)

    Supported values:
      ``local``      — SQLite (default, single-process)
      ``redis``      — Redis hash + sorted-set layout
      ``mysql``      — MySQL via asyncmy / SQLAlchemy
      ``postgresql`` — PostgreSQL via asyncpg / SQLAlchemy
    """
    backend = get_config("account.storage", "local").strip().lower()

    if backend == "local":
        return _make_local()
    if backend == "redis":
        return _make_redis()
    if backend in ("mysql", "mariadb"):
        return _make_sql("mysql")
    if backend in ("postgresql", "postgres", "pgsql"):
        return _make_sql("postgresql")

    raise ValueError(f"Unknown account storage backend: {backend!r}")


# ---------------------------------------------------------------------------
# Backend constructors
# ---------------------------------------------------------------------------

def _make_local() -> AccountRepository:
    from .local import LocalAccountRepository

    path_str = get_config("account.local.path", "data/accounts.db")
    db_path  = Path(path_str)
    if not db_path.is_absolute():
        # Resolve relative to project root.
        db_path = Path(__file__).resolve().parents[5] / db_path
    return LocalAccountRepository(db_path)


def _make_redis() -> AccountRepository:
    from redis.asyncio import Redis
    from .redis import RedisAccountRepository

    url = get_config("account.redis.url", "redis://localhost:6379/0")
    r   = Redis.from_url(url, decode_responses=False)
    return RedisAccountRepository(r)


def _make_sql(dialect: str) -> AccountRepository:
    from .sql import SqlAccountRepository, create_mysql_engine, create_pgsql_engine

    if dialect == "mysql":
        url    = get_config("account.mysql.url", "")
        engine = create_mysql_engine(url)
    else:
        url    = get_config("account.postgresql.url", "")
        engine = create_pgsql_engine(url)
    return SqlAccountRepository(engine, dialect=dialect)


__all__ = ["create_repository"]
