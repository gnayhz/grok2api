"""Admin token CRUD handlers."""

from __future__ import annotations

import asyncio
import re

from fastapi import APIRouter, Query
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.control.account.commands import AccountUpsert, BulkReplacePoolCommand, ListAccountsQuery
from app.control.account.repository import AccountRepository

router = APIRouter()

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_TOKEN_TRANS = str.maketrans({
    "\u2010": "-", "\u2011": "-", "\u2012": "-",
    "\u2013": "-", "\u2014": "-", "\u2212": "-",
    "\u00a0": " ", "\u2007": " ", "\u202f": " ",
    "\u200b": "", "\u200c": "", "\u200d": "", "\ufeff": "",
})


def _sanitize(value: str) -> str:
    tok = str(value or "").translate(_TOKEN_TRANS)
    tok = re.sub(r"\s+", "", tok)
    if tok.startswith("sso="):
        tok = tok[4:]
    return tok.encode("ascii", errors="ignore").decode("ascii")


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


def _get_repo() -> AccountRepository:
    try:
        from app.main import app as _app
        return _app.state.repository
    except Exception:
        from app.control.account.backends.factory import create_repository
        return create_repository()


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------

class AddTokensRequest(BaseModel):
    tokens: list[str]
    pool: str = "basic"
    tags: list[str] = []


class ReplacePoolRequest(BaseModel):
    pool: str
    tokens: list[str]
    tags: list[str] = []


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.post("/tokens")
async def save_tokens(data: dict):
    """Full pool replace — accepts {pool_name: [token_objects]} dict (legacy frontend format)."""
    repo = _get_repo()
    total_upserted = 0
    all_upserted_tokens: list[str] = []
    for pool_name, items in data.items():
        if not isinstance(items, list):
            continue
        upserts = []
        for item in items:
            token_data = {"token": item} if isinstance(item, str) else dict(item or {})
            token_val = _sanitize(token_data.get("token", ""))
            if not token_val:
                continue
            upserts.append(AccountUpsert(
                token = token_val,
                pool  = pool_name,
                tags  = token_data.get("tags") or [],
            ))
        if upserts:
            cmd = BulkReplacePoolCommand(pool=pool_name, upserts=upserts)
            await repo.replace_pool(cmd)
            all_upserted_tokens.extend(u.token for u in upserts)
            total_upserted += len(upserts)
    logger.info("Admin: saved {} tokens across pools", total_upserted)
    if all_upserted_tokens:
        asyncio.create_task(_refresh_imported(all_upserted_tokens))
    return JSONResponse({"status": "success", "count": total_upserted})


@router.delete("/tokens")
async def delete_tokens(tokens: list[str]):
    repo = _get_repo()
    cleaned = [t for t in (_sanitize(t) for t in tokens) if t]
    if not cleaned:
        return JSONResponse({"error": "No valid tokens provided"}, status_code=400)
    await repo.delete_accounts(cleaned)
    logger.info("Admin: deleted {} tokens", len(cleaned))
    return JSONResponse({"deleted": len(cleaned)})


@router.put("/tokens/pool")
async def replace_pool(req: ReplacePoolRequest):
    repo = _get_repo()
    cleaned = [t for t in (_sanitize(t) for t in req.tokens) if t]
    upserts = [AccountUpsert(token=t, pool=req.pool, tags=req.tags) for t in cleaned]
    cmd = BulkReplacePoolCommand(pool=req.pool, upserts=upserts)
    await repo.replace_pool(cmd)
    logger.info("Admin: replaced pool={} with {} tokens", req.pool, len(cleaned))
    if cleaned:
        asyncio.create_task(_refresh_imported(cleaned))
    return JSONResponse({"pool": req.pool, "count": len(cleaned)})


async def _refresh_imported(tokens: list[str]) -> None:
    """Fire-and-forget: fetch real quotas for newly imported tokens."""
    try:
        from app.control.account.refresh import AccountRefreshService
        svc = AccountRefreshService(_get_repo())
        await svc.refresh_on_import(tokens)
        logger.info("Import quota sync complete for {} tokens", len(tokens))
    except Exception as exc:
        logger.debug("Import quota sync failed: {}", exc)


def _quota_dict(r) -> dict:
    """Return {auto, fast, expert} remaining/total dict for frontend display."""
    q = getattr(r, "quota", None)
    if not isinstance(q, dict):
        return {}
    result = {}
    for mode in ("auto", "fast", "expert"):
        v = q.get(mode, {})
        if isinstance(v, dict):
            result[mode] = {"remaining": int(v.get("remaining", 0) or 0), "total": int(v.get("total", 0) or 0)}
    return result


@router.get("/tokens")
async def list_tokens():
    """Return all tokens grouped by pool (legacy format for frontend)."""
    repo = _get_repo()
    all_items = []
    page_num = 1
    while True:
        query = ListAccountsQuery(page=page_num, page_size=2000)
        page = await repo.list_accounts(query)
        all_items.extend(page.items)
        if page_num * 2000 >= page.total:
            break
        page_num += 1
    result: dict[str, list] = {}
    for r in all_items:
        pool = r.pool or "basic"
        result.setdefault(pool, []).append({
            "token":             r.token,
            "status":            r.status,
            "quota":             _quota_dict(r),
            "consumed":          0,
            "created_at":        r.created_at,
            "last_used_at":      r.last_use_at,
            "use_count":         r.usage_use_count or 0,
            "fail_count":        r.usage_fail_count or 0,
            "last_fail_at":      r.last_fail_at,
            "last_fail_reason":  r.last_fail_reason,
            "last_sync_at":      r.last_sync_at,
            "tags":              r.tags or [],
            "note":              getattr(r, "note", "") or "",
            "last_asset_clear_at": r.last_clear_at,
        })
    return JSONResponse({
        "tokens": result,
        "consumed_mode_enabled": False,
    })
