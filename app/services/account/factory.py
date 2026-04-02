"""
Factory helpers for the new account domain.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Optional

from pydantic import BaseModel, Field

from app.core.storage import DATA_DIR
from app.services.account.backends import (
    LocalAccountRepository,
    RedisAccountRepository,
    SQLAccountRepository,
)
from app.services.account.repository import AccountRepository
from app.services.account.storage_layout import (
    LOCAL_ACCOUNT_DB_NAME,
    LOCAL_ACCOUNT_SUBDIR,
    REDIS_ACCOUNT_NAMESPACE,
)


class AccountRepositorySettings(BaseModel):
    storage_type: str = Field(default="local")
    storage_url: str = Field(default="")
    data_dir: Path = Field(default=DATA_DIR)
    redis_namespace: str = Field(default=REDIS_ACCOUNT_NAMESPACE)
    local_subdir: Path = Field(default=LOCAL_ACCOUNT_SUBDIR)
    local_db_name: str = Field(default=LOCAL_ACCOUNT_DB_NAME)

    @classmethod
    def from_env(cls) -> "AccountRepositorySettings":
        storage_type = (
            os.getenv("ACCOUNT_STORAGE_TYPE")
            or os.getenv("SERVER_STORAGE_TYPE")
            or "local"
        ).lower()
        storage_url = (
            os.getenv("ACCOUNT_STORAGE_URL")
            or os.getenv("SERVER_STORAGE_URL")
            or ""
        )
        return cls(
            storage_type=storage_type,
            storage_url=storage_url,
            data_dir=Path(os.getenv("DATA_DIR", str(DATA_DIR))).expanduser(),
            redis_namespace=os.getenv(
                "ACCOUNT_REDIS_NAMESPACE", REDIS_ACCOUNT_NAMESPACE
            ),
            local_subdir=Path(
                os.getenv("ACCOUNT_LOCAL_SUBDIR", str(LOCAL_ACCOUNT_SUBDIR))
            ),
            local_db_name=os.getenv("ACCOUNT_LOCAL_DB_NAME", LOCAL_ACCOUNT_DB_NAME),
        )


def normalize_sql_url(storage_type: str, storage_url: str) -> str:
    if storage_type == "mysql" and storage_url.startswith("mysql://"):
        return storage_url.replace("mysql://", "mysql+aiomysql://", 1)
    if storage_type == "pgsql" and storage_url.startswith("postgresql://"):
        return storage_url.replace("postgresql://", "postgresql+asyncpg://", 1)
    if storage_type == "pgsql" and storage_url.startswith("postgres://"):
        return storage_url.replace("postgres://", "postgresql+asyncpg://", 1)
    return storage_url


def create_account_repository(settings: Optional[AccountRepositorySettings] = None) -> AccountRepository:
    settings = settings or AccountRepositorySettings.from_env()
    storage_type = settings.storage_type.lower()
    if storage_type == "local":
        return LocalAccountRepository(
            settings.data_dir / settings.local_subdir / settings.local_db_name
        )
    if storage_type == "redis":
        if not settings.storage_url:
            raise ValueError("Redis storage requires SERVER_STORAGE_URL")
        return RedisAccountRepository(settings.storage_url, namespace=settings.redis_namespace)
    if storage_type in {"mysql", "pgsql"}:
        if not settings.storage_url:
            raise ValueError("SQL storage requires SERVER_STORAGE_URL")
        return SQLAccountRepository(normalize_sql_url(storage_type, settings.storage_url))
    raise ValueError(f"Unsupported account storage type: {settings.storage_type}")
