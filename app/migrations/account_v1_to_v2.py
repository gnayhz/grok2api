"""Migrate accounts from v1 (single-integer quota) to v2 (per-mode quota windows).

v1 schema (``data/account/v1/accounts.db``):
    pool_name TEXT, token TEXT, quota INTEGER, consumed INTEGER

v2 schema: current ``LocalAccountRepository`` (per-mode quota columns).
"""

from __future__ import annotations

import sqlite3
from contextlib import closing
from pathlib import Path
from typing import Any

from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.control.account.commands import AccountUpsert
from app.control.account.backends.factory import create_repository


_BASE_DIR = Path(__file__).resolve().parents[2]  # project root
_V1_DB_PATH = _BASE_DIR / "data" / "account" / "v1" / "accounts.db"


class V1MigrationReport(BaseModel):
    source: str = ""
    discovered: int = 0
    imported: int = 0
    skipped: int = 0
    revision: int = 0
    already_done: bool = False


def _detect_v1_db(path: Path | None = None) -> Path | None:
    """Return the v1 DB path if it exists, else None."""
    p = path or _V1_DB_PATH
    return p if p.exists() else None


def _read_v1_accounts(db_path: Path) -> list[dict[str, Any]]:
    """Read all rows from the v1 accounts table."""
    rows: list[dict[str, Any]] = []
    try:
        with closing(sqlite3.connect(db_path)) as conn:
            conn.row_factory = sqlite3.Row
            cursor = conn.execute(
                "SELECT pool_name, token, quota, consumed FROM accounts"
            )
            for row in cursor:
                rows.append(dict(row))
    except Exception as exc:
        logger.warning("v1→v2 migration: failed to read {}: {}", db_path, exc)
    return rows


def _convert_rows(rows: list[dict[str, Any]]) -> list[AccountUpsert]:
    """Convert v1 rows to AccountUpsert commands."""
    items: list[AccountUpsert] = []
    for row in rows:
        token = (row.get("token") or "").strip()
        if not token:
            continue
        pool_raw = (row.get("pool_name") or "basic").strip().lower()
        pool = "super" if pool_raw in ("super", "ssosuper") else "basic"
        items.append(AccountUpsert(token=token, pool=pool))
    return items


async def run_v1_to_v2_migration(
    *,
    v1_path: Path | None = None,
    force: bool = False,
    batch_size: int = 500,
) -> V1MigrationReport:
    """Migrate v1 accounts into the current v2 repository."""
    db_path = _detect_v1_db(v1_path)
    if db_path is None:
        return V1MigrationReport(source="(not found)")

    repo = create_repository()
    await repo.initialize()

    snapshot = await repo.runtime_snapshot()
    if snapshot.items and not force:
        await repo.close()
        return V1MigrationReport(
            source=str(db_path),
            revision=snapshot.revision,
            already_done=True,
        )

    rows = _read_v1_accounts(db_path)
    items = _convert_rows(rows)
    imported = 0
    for start in range(0, len(items), batch_size):
        chunk = items[start : start + batch_size]
        result = await repo.upsert_accounts(chunk)
        imported += result.upserted

    revision = await repo.get_revision()
    await repo.close()

    logger.info(
        "v1→v2 migration complete: discovered={} imported={} skipped={}",
        len(items), imported, max(0, len(items) - imported),
    )
    return V1MigrationReport(
        source=str(db_path),
        discovered=len(items),
        imported=imported,
        skipped=max(0, len(items) - imported),
        revision=revision,
    )


__all__ = ["run_v1_to_v2_migration", "V1MigrationReport"]
