"""
Migration helpers for moving legacy token storage into the new account domain.
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import aiofiles
import orjson
from pydantic import BaseModel

from app.core.logger import logger
from app.core.storage import DATA_DIR, StorageFactory
from app.services.account.commands import AccountUpsert
from app.services.account.factory import (
    AccountRepositorySettings,
    create_account_repository,
)
from app.services.account.models import AccountStatus, now_ms
from app.services.account.storage_layout import ACCOUNT_SCHEMA_VERSION

LEGACY_TOKEN_FILE = "token.json"
LEGACY_REDIS_POOLS_KEY = "grok2api:pools"
LEGACY_REDIS_POOL_PREFIX = "grok2api:pool:"
LEGACY_REDIS_TOKEN_PREFIX = "grok2api:token:"


class AccountMigrationReport(BaseModel):
    source_storage_type: str
    target_storage_type: str
    discovered: int = 0
    imported: int = 0
    skipped: int = 0
    revision: int = 0
    already_initialized: bool = False


def _normalize_legacy_token_item(pool_name: str, raw: Any) -> AccountUpsert | None:
    if isinstance(raw, str):
        return AccountUpsert(token=raw, pool_name=pool_name)
    if not isinstance(raw, dict):
        return None

    payload = dict(raw)
    token = payload.get("token")
    if not token:
        return None

    status = payload.get("status", AccountStatus.ACTIVE)
    if isinstance(status, str) and status.startswith("TokenStatus."):
        status = status.split(".", 1)[1].lower()

    tags = payload.get("tags") or []
    if isinstance(tags, str):
        try:
            parsed = orjson.loads(tags)
            tags = parsed if isinstance(parsed, list) else [tags]
        except Exception:
            tags = [tag.strip() for tag in tags.split(",") if tag.strip()]

    return AccountUpsert(
        token=token,
        pool_name=pool_name,
        status=status,
        quota=int(payload.get("quota", 80) or 0),
        consumed=int(payload.get("consumed", 0) or 0),
        created_at=payload.get("created_at"),
        last_used_at=payload.get("last_used_at"),
        use_count=int(payload.get("use_count", 0) or 0),
        fail_count=int(payload.get("fail_count", 0) or 0),
        last_fail_at=payload.get("last_fail_at"),
        last_fail_reason=payload.get("last_fail_reason"),
        last_sync_at=payload.get("last_sync_at"),
        tags=tags,
        note=payload.get("note") or "",
        last_asset_clear_at=payload.get("last_asset_clear_at"),
        metadata={},
    )


async def _load_legacy_local_tokens() -> dict[str, Any]:
    token_file = Path(
        os.getenv("ACCOUNT_MIGRATION_SOURCE_FILE", str(DATA_DIR / LEGACY_TOKEN_FILE))
    ).expanduser()
    if not token_file.exists():
        return {}
    try:
        async with aiofiles.open(token_file, "rb") as file:
            payload = await file.read()
        data = orjson.loads(payload)
        return data if isinstance(data, dict) else {}
    except Exception as error:
        logger.warning("Account migration: failed to read local token file: {}", error)
        return {}


async def _load_legacy_redis_tokens(storage_url: str) -> dict[str, Any]:
    if not storage_url:
        return {}
    try:
        from redis import asyncio as aioredis
    except ImportError:
        logger.warning("Account migration: redis package unavailable")
        return {}

    redis = aioredis.from_url(storage_url, decode_responses=True, health_check_interval=30)
    try:
        pool_names = await redis.smembers(LEGACY_REDIS_POOLS_KEY)
        if not pool_names:
            return {}

        pools: dict[str, list[dict[str, Any]]] = {}
        async with redis.pipeline() as pipe:
            for pool_name in pool_names:
                pipe.smembers(f"{LEGACY_REDIS_POOL_PREFIX}{pool_name}")
            pool_token_sets = await pipe.execute()

        all_token_ids: list[str] = []
        pool_map: dict[str, list[str]] = {}
        for index, pool_name in enumerate(pool_names):
            token_ids = list(pool_token_sets[index] or [])
            pool_map[pool_name] = token_ids
            all_token_ids.extend(token_ids)

        token_lookup: dict[str, dict[str, Any]] = {}
        if all_token_ids:
            async with redis.pipeline() as pipe:
                for token_id in all_token_ids:
                    pipe.hgetall(f"{LEGACY_REDIS_TOKEN_PREFIX}{token_id}")
                token_payloads = await pipe.execute()

            for index, token_id in enumerate(all_token_ids):
                item = token_payloads[index]
                if not item:
                    continue
                normalized = dict(item)
                for key in (
                    "quota",
                    "consumed",
                    "created_at",
                    "last_used_at",
                    "use_count",
                    "fail_count",
                    "last_fail_at",
                    "last_sync_at",
                    "last_asset_clear_at",
                ):
                    if normalized.get(key) not in (None, "", "None"):
                        try:
                            normalized[key] = int(normalized[key])
                        except Exception:
                            pass
                if "tags" in normalized:
                    try:
                        parsed = orjson.loads(normalized["tags"])
                        normalized["tags"] = parsed if isinstance(parsed, list) else []
                    except Exception:
                        normalized["tags"] = []
                token_lookup[token_id] = normalized

        for pool_name, token_ids in pool_map.items():
            pools[pool_name] = [
                token_lookup[token_id]
                for token_id in token_ids
                if token_id in token_lookup
            ]
        return pools
    except Exception as error:
        logger.warning("Account migration: failed to read legacy redis tokens: {}", error)
        return {}
    finally:
        try:
            await redis.close()
        except Exception:
            pass


async def _load_legacy_sql_tokens(storage_type: str, storage_url: str) -> dict[str, Any]:
    if not storage_url:
        return {}
    try:
        from sqlalchemy import text
        from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine
    except ImportError:
        logger.warning("Account migration: sqlalchemy async support unavailable")
        return {}

    normalized_url, connect_args = StorageFactory._prepare_sql_url_and_connect_args(
        storage_type,
        storage_url,
    )
    engine = create_async_engine(
        normalized_url,
        echo=False,
        pool_size=5,
        max_overflow=5,
        pool_recycle=3600,
        pool_pre_ping=True,
        **({"connect_args": connect_args} if connect_args else {}),
    )
    session_factory = async_sessionmaker(engine, expire_on_commit=False)

    try:
        async with session_factory() as session:
            result = await session.execute(
                text(
                    "SELECT token, pool_name, status, quota, created_at, "
                    "last_used_at, use_count, fail_count, last_fail_at, "
                    "last_fail_reason, last_sync_at, tags, note, "
                    "last_asset_clear_at, data "
                    "FROM tokens"
                )
            )
            rows = result.fetchall()
    except Exception as error:
        logger.warning("Account migration: failed to read legacy sql tokens: {}", error)
        await engine.dispose()
        return {}

    pools: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        (
            token,
            pool_name,
            status,
            quota,
            created_at,
            last_used_at,
            use_count,
            fail_count,
            last_fail_at,
            last_fail_reason,
            last_sync_at,
            tags,
            note,
            last_asset_clear_at,
            data_json,
        ) = row

        payload: dict[str, Any] = {"token": token}
        if status is not None:
            payload["status"] = status.value if hasattr(status, "value") else status
        if quota is not None:
            payload["quota"] = int(quota)
        if created_at is not None:
            payload["created_at"] = int(created_at)
        if last_used_at is not None:
            payload["last_used_at"] = int(last_used_at)
        if use_count is not None:
            payload["use_count"] = int(use_count)
        if fail_count is not None:
            payload["fail_count"] = int(fail_count)
        if last_fail_at is not None:
            payload["last_fail_at"] = int(last_fail_at)
        if last_fail_reason is not None:
            payload["last_fail_reason"] = last_fail_reason
        if last_sync_at is not None:
            payload["last_sync_at"] = int(last_sync_at)
        if tags is not None:
            payload["tags"] = tags
        if note is not None:
            payload["note"] = note
        if last_asset_clear_at is not None:
            payload["last_asset_clear_at"] = int(last_asset_clear_at)

        if data_json:
            try:
                legacy_payload = orjson.loads(data_json) if isinstance(data_json, str) else data_json
                if isinstance(legacy_payload, dict):
                    for key, value in legacy_payload.items():
                        payload.setdefault(key, value)
            except Exception:
                pass

        pools.setdefault(pool_name, []).append(payload)

    await engine.dispose()
    return pools


async def load_legacy_tokens_from_source(
    *,
    source_type: str | None = None,
    source_url: str | None = None,
) -> list[AccountUpsert]:
    resolved_type = (
        source_type
        or os.getenv("ACCOUNT_MIGRATION_SOURCE_TYPE")
        or os.getenv("SERVER_STORAGE_TYPE")
        or "local"
    ).lower()
    resolved_url = (
        source_url
        if source_url is not None
        else os.getenv("ACCOUNT_MIGRATION_SOURCE_URL")
        or os.getenv("SERVER_STORAGE_URL")
        or ""
    )

    if resolved_type == "local":
        data = await _load_legacy_local_tokens()
    elif resolved_type == "redis":
        data = await _load_legacy_redis_tokens(resolved_url)
    elif resolved_type in {"mysql", "pgsql"}:
        data = await _load_legacy_sql_tokens(resolved_type, resolved_url)
    else:
        logger.warning("Account migration: unsupported legacy source type '{}'", resolved_type)
        data = {}

    items: list[AccountUpsert] = []
    for pool_name, token_items in data.items():
        if not isinstance(token_items, list):
            continue
        for raw in token_items:
            normalized = _normalize_legacy_token_item(pool_name, raw)
            if normalized is not None:
                items.append(normalized)
    return items


async def migrate_legacy_tokens_to_accounts(
    *,
    settings: AccountRepositorySettings | None = None,
    force: bool = False,
    batch_size: int = 1000,
) -> AccountMigrationReport:
    settings = settings or AccountRepositorySettings.from_env()
    repository = create_account_repository(settings)
    await repository.initialize()
    source_storage_type = (
        os.getenv("ACCOUNT_MIGRATION_SOURCE_TYPE")
        or os.getenv("SERVER_STORAGE_TYPE")
        or "local"
    ).lower()

    current_metadata = await repository.get_metadata()
    snapshot = await repository.runtime_snapshot(include_deleted=True)
    if (
        snapshot.items or current_metadata.get("legacy_migration_completed") == "1"
    ) and not force:
        await repository.close()
        return AccountMigrationReport(
            source_storage_type=source_storage_type,
            target_storage_type=settings.storage_type,
            discovered=0,
            imported=0,
            skipped=0,
            revision=snapshot.revision,
            already_initialized=True,
        )

    items = await load_legacy_tokens_from_source(source_type=source_storage_type)
    imported = 0
    for start in range(0, len(items), batch_size):
        chunk = items[start : start + batch_size]
        result = await repository.upsert_accounts(chunk)
        imported += result.upserted

    await repository.set_metadata(
        {
            "schema_version": ACCOUNT_SCHEMA_VERSION,
            "legacy_migration_completed": "1",
            "legacy_migration_completed_at": str(now_ms()),
            "legacy_migration_item_count": str(imported),
        }
    )
    revision = await repository.get_revision()
    await repository.close()
    return AccountMigrationReport(
        source_storage_type=source_storage_type,
        target_storage_type=settings.storage_type,
        discovered=len(items),
        imported=imported,
        skipped=max(0, len(items) - imported),
        revision=revision,
        already_initialized=False,
    )
