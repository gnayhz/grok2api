"""Batch operations — NSFW enable, usage refresh, online cache clear.

Supports both sync (default) and async mode (``?async=true`` → returns task_id).
"""

from __future__ import annotations

import asyncio

from fastapi import APIRouter, HTTPException, Query
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.platform.runtime.task import AsyncTask, create_task, expire_task
from app.control.account.commands import ListAccountsQuery
from app.control.account.repository import AccountRepository

router = APIRouter(prefix="/batch")

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _get_repo() -> AccountRepository:
    try:
        from app.main import app as _app
        return _app.state.repository
    except Exception:
        from app.control.account.backends.factory import create_repository
        return create_repository()


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


async def _list_all_tokens() -> list[str]:
    repo = _get_repo()
    page_num, tokens = 1, []
    while True:
        page = await repo.list_accounts(ListAccountsQuery(page=page_num, page_size=2000))
        tokens.extend(r.token for r in page.items)
        if page_num * 2000 >= page.total:
            break
        page_num += 1
    return tokens


class BatchRequest(BaseModel):
    tokens: list[str] = []


async def _dispatch_batch(
    tokens: list[str],
    handler,
    *,
    use_async: bool,
    label: str,
):
    """Run *handler* per token sync or async; return appropriate response."""
    if use_async:
        task = create_task(len(tokens))

        async def _run():
            try:
                results, ok_c, fail_c = {}, 0, 0
                for token in tokens:
                    if task.cancelled:
                        task.finish_cancelled()
                        return
                    try:
                        data = await handler(token)
                        ok_c += 1
                        results[_mask(token)] = data
                        task.record(True)
                    except Exception as exc:
                        fail_c += 1
                        results[_mask(token)] = {"error": str(exc)}
                        task.record(False, error=str(exc))
                task.finish({
                    "status": "success",
                    "summary": {"total": len(tokens), "ok": ok_c, "fail": fail_c},
                    "results": results,
                })
            except Exception as exc:
                task.fail_task(str(exc))
            finally:
                asyncio.create_task(expire_task(task.id, 300))

        asyncio.create_task(_run())
        return JSONResponse({"status": "success", "task_id": task.id, "total": len(tokens)})

    # Sync path
    results, ok_c, fail_c = {}, 0, 0
    for token in tokens:
        try:
            data = await handler(token)
            ok_c += 1
            results[_mask(token)] = data
        except Exception as exc:
            fail_c += 1
            results[_mask(token)] = {"error": str(exc)}
    return JSONResponse({
        "status": "success",
        "summary": {"total": len(tokens), "ok": ok_c, "fail": fail_c},
        "results": results,
    })


# ---------------------------------------------------------------------------
# Per-token handlers
# ---------------------------------------------------------------------------

async def _nsfw_one(token: str) -> dict:
    from app.dataplane.reverse.protocol.xai_auth import accept_tos, set_birth_date, enable_nsfw
    await accept_tos(token)
    await set_birth_date(token)
    await enable_nsfw(token)
    return {"success": True}


async def _refresh_one(token: str) -> dict:
    from app.control.account.refresh import AccountRefreshService
    repo = _get_repo()
    svc = AccountRefreshService(repo)
    result = await svc.refresh_tokens([token])
    return {"refreshed": result.refreshed, "failed": result.failed}


async def _cache_clear_one(token: str) -> dict:
    from app.dataplane.reverse.transport.assets import list_assets, delete_asset
    assets_resp = await list_assets(token)
    items = assets_resp.get("assets", assets_resp.get("items", []))
    deleted = 0
    for item in items:
        asset_id = item.get("id") or item.get("assetId")
        if asset_id:
            await delete_asset(token, asset_id)
            deleted += 1
    return {"deleted": deleted}


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.post("/nsfw")
async def batch_nsfw(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        tokens = await _list_all_tokens()
    if not tokens:
        raise HTTPException(status_code=400, detail="No tokens available")
    return await _dispatch_batch(tokens, _nsfw_one, use_async=async_mode, label="nsfw")


@router.post("/refresh")
async def batch_refresh(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        raise HTTPException(status_code=400, detail="No tokens provided")
    return await _dispatch_batch(tokens, _refresh_one, use_async=async_mode, label="refresh")


@router.post("/cache-clear")
async def batch_cache_clear(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        raise HTTPException(status_code=400, detail="No tokens provided")
    return await _dispatch_batch(tokens, _cache_clear_one, use_async=async_mode, label="cache-clear")
