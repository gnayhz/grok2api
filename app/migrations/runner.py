"""Migration runner — import legacy token storage into the new account domain."""

from __future__ import annotations

import argparse
import asyncio
import os
from pathlib import Path
from typing import Any

import orjson
from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.control.account.commands import AccountUpsert
from app.control.account.backends.factory import create_repository

# ---------------------------------------------------------------------------
# Legacy storage keys
# ---------------------------------------------------------------------------
_LEGACY_FILE          = "token.json"
_LEGACY_REDIS_POOLS   = "grok2api:pools"
_LEGACY_REDIS_POOL    = "grok2api:pool:"
_LEGACY_REDIS_TOKEN   = "grok2api:token:"


class MigrationReport(BaseModel):
    source: str
    target: str
    discovered: int = 0
    imported:   int = 0
    skipped:    int = 0
    revision:   int = 0
    already_done: bool = False


# ---------------------------------------------------------------------------
# Loaders
# ---------------------------------------------------------------------------

def _normalize(pool: str, raw: Any) -> AccountUpsert | None:
    if isinstance(raw, str):
        return AccountUpsert(token=raw, pool=pool) if raw else None
    if not isinstance(raw, dict):
        return None
    token = raw.get("token")
    if not token:
        return None
    tags = raw.get("tags") or []
    if isinstance(tags, str):
        try:
            tags = orjson.loads(tags)
        except Exception:
            tags = [t.strip() for t in tags.split(",") if t.strip()]
    return AccountUpsert(
        token = token,
        pool  = pool,
        tags  = tags if isinstance(tags, list) else [],
        ext   = {k: v for k, v in raw.items() if k not in ("token", "pool", "tags")},
    )


async def _load_local(path: str) -> dict[str, list]:
    p = Path(path)
    if not p.exists():
        return {}
    try:
        data = orjson.loads(p.read_bytes())
        return data if isinstance(data, dict) else {}
    except Exception as exc:
        logger.warning("Migration: failed to read local file {}: {}", path, exc)
        return {}


async def _load_redis(url: str) -> dict[str, list]:
    try:
        from redis import asyncio as aioredis
    except ImportError:
        logger.warning("Migration: redis package unavailable")
        return {}
    r = aioredis.from_url(url, decode_responses=True)
    try:
        pool_names = await r.smembers(_LEGACY_REDIS_POOLS)
        if not pool_names:
            return {}
        result: dict[str, list] = {}
        for pool_name in pool_names:
            token_ids = await r.smembers(f"{_LEGACY_REDIS_POOL}{pool_name}")
            items = []
            for tid in token_ids:
                d = await r.hgetall(f"{_LEGACY_REDIS_TOKEN}{tid}")
                if d:
                    items.append(d)
            result[pool_name] = items
        return result
    except Exception as exc:
        logger.warning("Migration: redis read failed: {}", exc)
        return {}
    finally:
        await r.aclose()


async def _load_source(source_type: str, source_url: str) -> list[AccountUpsert]:
    if source_type == "local":
        data = await _load_local(
            os.getenv("ACCOUNT_MIGRATION_SOURCE_FILE", str(Path.cwd() / "data" / _LEGACY_FILE))
        )
    elif source_type == "redis":
        data = await _load_redis(source_url)
    else:
        logger.warning("Migration: unsupported source type '{}'", source_type)
        return []

    items: list[AccountUpsert] = []
    for pool_name, token_list in data.items():
        if not isinstance(token_list, list):
            continue
        for raw in token_list:
            normalized = _normalize(pool_name, raw)
            if normalized:
                items.append(normalized)
    return items


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

async def run_migration(
    *,
    force:       bool = False,
    source_type: str  = "local",
    source_url:  str  = "",
    batch_size:  int  = 1000,
) -> MigrationReport:
    # Auto-detect v1 SQLite DB and migrate first.
    from .account_v1_to_v2 import run_v1_to_v2_migration
    v1_report = await run_v1_to_v2_migration(force=force)
    if v1_report.imported > 0:
        logger.info("v1→v2 auto-migration imported {} accounts", v1_report.imported)

    repo = create_repository()
    await repo.initialize()

    target = os.getenv("ACCOUNT_STORAGE", "local")
    snapshot = await repo.runtime_snapshot()
    if snapshot.items and not force:
        await repo.close()
        return MigrationReport(
            source     = source_type,
            target     = target,
            revision   = snapshot.revision,
            already_done = True,
        )

    items = await _load_source(source_type, source_url)
    imported = 0
    for start in range(0, len(items), batch_size):
        chunk  = items[start : start + batch_size]
        result = await repo.upsert_accounts(chunk)
        imported += result.upserted

    revision = await repo.get_revision()
    await repo.close()

    return MigrationReport(
        source     = source_type,
        target     = target,
        discovered = len(items),
        imported   = imported,
        skipped    = max(0, len(items) - imported),
        revision   = revision,
    )


async def _main() -> int:
    parser = argparse.ArgumentParser(description="Grok2API storage migration")
    parser.add_argument("--force",       action="store_true")
    parser.add_argument("--source-type", default=os.getenv("ACCOUNT_MIGRATION_SOURCE_TYPE", "local"))
    parser.add_argument("--source-url",  default=os.getenv("ACCOUNT_MIGRATION_SOURCE_URL", ""))
    args = parser.parse_args()

    report = await run_migration(
        force       = args.force,
        source_type = args.source_type,
        source_url  = args.source_url,
    )
    print(report.model_dump_json(indent=2))
    return 0


def main() -> int:
    return asyncio.run(_main())


if __name__ == "__main__":
    raise SystemExit(main())
