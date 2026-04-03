"""Admin token CRUD — list, import, delete, replace pool.

Performance notes:
  - DI-injected repo (no try/except per call)
  - orjson direct output (bypasses stdlib json)
  - Quota dict: zero deserialization — reads r.quota directly
  - Import refresh: reuses app.state.refresh_service singleton
"""

from __future__ import annotations

import asyncio
import re
from typing import TYPE_CHECKING

import orjson
from fastapi import APIRouter, Depends
from fastapi.responses import Response
from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.control.account.commands import (
    AccountUpsert,
    BulkReplacePoolCommand,
    ListAccountsQuery,
)

if TYPE_CHECKING:
    from app.control.account.refresh import AccountRefreshService
    from app.control.account.repository import AccountRepository

from . import get_refresh_svc, get_repo

router = APIRouter()

# ---------------------------------------------------------------------------
# Token sanitisation
# ---------------------------------------------------------------------------

_TOKEN_TRANS = str.maketrans({
    "\u2010": "-", "\u2011": "-", "\u2012": "-",
    "\u2013": "-", "\u2014": "-", "\u2212": "-",
    "\u00a0": " ", "\u2007": " ", "\u202f": " ",
    "\u200b": "", "\u200c": "", "\u200d": "", "\ufeff": "",
})
_STRIP_RE = re.compile(r"\s+")


def _sanitize(value: str) -> str:
    tok = str(value or "").translate(_TOKEN_TRANS)
    tok = _STRIP_RE.sub("", tok)
    if tok.startswith("sso="):
        tok = tok[4:]
    return tok.encode("ascii", errors="ignore").decode("ascii")


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------

class ReplacePoolRequest(BaseModel):
    pool: str
    tokens: list[str]
    tags: list[str] = []


# ---------------------------------------------------------------------------
# Serialisation — zero-copy quota extraction
# ---------------------------------------------------------------------------

def _quota_brief(q: dict) -> dict:
    """Extract {auto, fast, expert} with only remaining/total from stored quota dict."""
    out = {}
    for mode in ("auto", "fast", "expert"):
        v = q.get(mode)
        if isinstance(v, dict):
            out[mode] = {
                "remaining": int(v.get("remaining", 0) or 0),
                "total": int(v.get("total", 0) or 0),
            }
    return out


def _serialize_record(r, pool: str) -> dict:
    return {
        "token":              r.token,
        "status":             r.status,
        "quota":              _quota_brief(r.quota) if isinstance(r.quota, dict) else {},
        "consumed":           0,
        "created_at":         r.created_at,
        "last_used_at":       r.last_use_at,
        "use_count":          r.usage_use_count or 0,
        "fail_count":         r.usage_fail_count or 0,
        "last_fail_at":       r.last_fail_at,
        "last_fail_reason":   r.last_fail_reason,
        "last_sync_at":       r.last_sync_at,
        "tags":               r.tags or [],
        "note":               getattr(r, "note", "") or "",
        "last_asset_clear_at": r.last_clear_at,
    }


def _json(data) -> Response:
    """orjson fast-path response."""
    return Response(content=orjson.dumps(data), media_type="application/json")


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.get("/tokens")
async def list_tokens(repo: "AccountRepository" = Depends(get_repo)):
    """Return all tokens grouped by pool."""
    all_items: list = []
    page_num = 1
    while True:
        page = await repo.list_accounts(ListAccountsQuery(page=page_num, page_size=2000))
        all_items.extend(page.items)
        if page_num * 2000 >= page.total:
            break
        page_num += 1

    result: dict[str, list] = {}
    for r in all_items:
        pool = r.pool or "basic"
        result.setdefault(pool, []).append(_serialize_record(r, pool))

    return _json({"tokens": result, "consumed_mode_enabled": False})


@router.post("/tokens")
async def save_tokens(
    data: dict,
    repo: "AccountRepository" = Depends(get_repo),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    """Full pool replace — accepts {pool_name: [token_objects]} dict."""
    total_upserted = 0
    all_tokens: list[str] = []

    for pool_name, items in data.items():
        if not isinstance(items, list):
            continue
        upserts = []
        for item in items:
            td = {"token": item} if isinstance(item, str) else dict(item or {})
            token_val = _sanitize(td.get("token", ""))
            if not token_val:
                continue
            upserts.append(AccountUpsert(token=token_val, pool=pool_name, tags=td.get("tags") or []))
        if upserts:
            await repo.replace_pool(BulkReplacePoolCommand(pool=pool_name, upserts=upserts))
            all_tokens.extend(u.token for u in upserts)
            total_upserted += len(upserts)

    logger.info("Admin: saved {} tokens across pools", total_upserted)
    if all_tokens:
        asyncio.create_task(_refresh_imported(refresh_svc, all_tokens))
    return _json({"status": "success", "count": total_upserted})


@router.delete("/tokens")
async def delete_tokens(
    tokens: list[str],
    repo: "AccountRepository" = Depends(get_repo),
):
    cleaned = [t for t in (_sanitize(t) for t in tokens) if t]
    if not cleaned:
        return Response(
            content=orjson.dumps({"error": "No valid tokens provided"}),
            media_type="application/json",
            status_code=400,
        )
    await repo.delete_accounts(cleaned)
    logger.info("Admin: deleted {} tokens", len(cleaned))
    return _json({"deleted": len(cleaned)})


@router.put("/tokens/pool")
async def replace_pool(
    req: ReplacePoolRequest,
    repo: "AccountRepository" = Depends(get_repo),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    cleaned = [t for t in (_sanitize(t) for t in req.tokens) if t]
    upserts = [AccountUpsert(token=t, pool=req.pool, tags=req.tags) for t in cleaned]
    await repo.replace_pool(BulkReplacePoolCommand(pool=req.pool, upserts=upserts))
    logger.info("Admin: replaced pool={} with {} tokens", req.pool, len(cleaned))
    if cleaned:
        asyncio.create_task(_refresh_imported(refresh_svc, cleaned))
    return _json({"pool": req.pool, "count": len(cleaned)})


# ---------------------------------------------------------------------------
# Fire-and-forget import refresh
# ---------------------------------------------------------------------------

async def _refresh_imported(svc: "AccountRefreshService", tokens: list[str]) -> None:
    try:
        await svc.refresh_on_import(tokens)
        logger.info("Import quota sync done for {} tokens", len(tokens))
    except Exception as exc:
        logger.debug("Import quota sync failed: {}", exc)
