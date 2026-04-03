"""Admin batch operations + SSE progress streaming.

Performance notes:
  - Uses ``run_batch`` for bounded-concurrency parallel execution
    (replaces old sequential for-loop)
  - Async mode: background task with SSE fan-out via AsyncTask
  - Sync mode: concurrent execution, single JSON response
"""

from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING, Any, Callable, Awaitable

import orjson
from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import Response, StreamingResponse
from pydantic import BaseModel

from app.platform.logging.logger import logger
from app.platform.runtime.batch import run_batch
from app.platform.runtime.task import AsyncTask, create_task, expire_task, get_task
from app.platform.auth.middleware import get_admin_key
from app.control.account.commands import ListAccountsQuery

if TYPE_CHECKING:
    from app.control.account.refresh import AccountRefreshService
    from app.control.account.repository import AccountRepository

from . import get_refresh_svc, get_repo

router = APIRouter(prefix="/batch")

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _mask(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


async def _list_all_tokens(repo: "AccountRepository") -> list[str]:
    page_num, tokens = 1, []
    while True:
        page = await repo.list_accounts(ListAccountsQuery(page=page_num, page_size=2000))
        tokens.extend(r.token for r in page.items)
        if page_num * 2000 >= page.total:
            break
        page_num += 1
    return tokens


def _json(data: Any, status_code: int = 200) -> Response:
    return Response(content=orjson.dumps(data), media_type="application/json", status_code=status_code)


class BatchRequest(BaseModel):
    tokens: list[str] = []


# ---------------------------------------------------------------------------
# Dispatch engine — sync (run_batch) or async (background task + SSE)
# ---------------------------------------------------------------------------

async def _dispatch(
    tokens: list[str],
    handler: Callable[[str], Awaitable[dict]],
    *,
    use_async: bool,
    concurrency: int = 10,
) -> Response:
    if use_async:
        return await _dispatch_async(tokens, handler, concurrency)
    return await _dispatch_sync(tokens, handler, concurrency)


async def _dispatch_sync(
    tokens: list[str],
    handler: Callable[[str], Awaitable[dict]],
    concurrency: int,
) -> Response:
    """Concurrent execution, collect all results, return at once."""
    results: dict[str, Any] = {}
    ok_c = fail_c = 0

    async def _wrapped(token: str) -> tuple[str, dict | None, str | None]:
        try:
            data = await handler(token)
            return token, data, None
        except Exception as exc:
            return token, None, str(exc)

    raw = await run_batch(tokens, _wrapped, concurrency=concurrency)
    for token, data, err in raw:
        key = _mask(token)
        if err is None:
            ok_c += 1
            results[key] = data
        else:
            fail_c += 1
            results[key] = {"error": err}

    return _json({
        "status": "success",
        "summary": {"total": len(tokens), "ok": ok_c, "fail": fail_c},
        "results": results,
    })


async def _dispatch_async(
    tokens: list[str],
    handler: Callable[[str], Awaitable[dict]],
    concurrency: int,
) -> Response:
    """Background task with per-item progress via AsyncTask SSE."""
    task = create_task(len(tokens))

    async def _run() -> None:
        try:
            sem = asyncio.Semaphore(concurrency)
            results: dict[str, Any] = {}
            ok_c = fail_c = 0

            async def _one(token: str) -> None:
                nonlocal ok_c, fail_c
                if task.cancelled:
                    return
                async with sem:
                    try:
                        data = await handler(token)
                        ok_c += 1
                        results[_mask(token)] = data
                        task.record(True)
                    except Exception as exc:
                        fail_c += 1
                        results[_mask(token)] = {"error": str(exc)}
                        task.record(False, error=str(exc))

            await asyncio.gather(*[_one(t) for t in tokens])

            if task.cancelled:
                task.finish_cancelled()
            else:
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
    return _json({"status": "success", "task_id": task.id, "total": len(tokens)})


# ---------------------------------------------------------------------------
# Per-token handlers
# ---------------------------------------------------------------------------

async def _nsfw_one(token: str) -> dict:
    from app.dataplane.reverse.protocol.xai_auth import accept_tos, set_birth_date, enable_nsfw
    await accept_tos(token)
    await set_birth_date(token)
    await enable_nsfw(token)
    return {"success": True}


async def _cache_clear_one(token: str) -> dict:
    from app.dataplane.reverse.transport.assets import list_assets, delete_asset
    resp = await list_assets(token)
    items = resp.get("assets", resp.get("items", []))
    deleted = 0
    for item in items:
        aid = item.get("id") or item.get("assetId")
        if aid:
            await delete_asset(token, aid)
            deleted += 1
    return {"deleted": deleted}


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.post("/nsfw")
async def batch_nsfw(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
    repo: "AccountRepository" = Depends(get_repo),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        tokens = await _list_all_tokens(repo)
    if not tokens:
        raise HTTPException(400, "No tokens available")
    return await _dispatch(tokens, _nsfw_one, use_async=async_mode)


@router.post("/refresh")
async def batch_refresh(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
    refresh_svc: "AccountRefreshService" = Depends(get_refresh_svc),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        raise HTTPException(400, "No tokens provided")

    async def _refresh_one(token: str) -> dict:
        result = await refresh_svc.refresh_tokens([token])
        return {"refreshed": result.refreshed, "failed": result.failed}

    return await _dispatch(tokens, _refresh_one, use_async=async_mode)


@router.post("/cache-clear")
async def batch_cache_clear(
    req: BatchRequest,
    async_mode: bool = Query(False, alias="async"),
):
    tokens = [t.strip() for t in req.tokens if t.strip()]
    if not tokens:
        raise HTTPException(400, "No tokens provided")
    return await _dispatch(tokens, _cache_clear_one, use_async=async_mode)


# ---------------------------------------------------------------------------
# SSE stream + cancel
# ---------------------------------------------------------------------------

@router.get("/{task_id}/stream")
async def batch_stream(task_id: str, request: Request):
    # Auth via query param for EventSource (Bearer header unavailable).
    app_key = get_admin_key()
    if app_key:
        key = request.query_params.get("app_key")
        if key != app_key:
            raise HTTPException(401, "Invalid authentication token")

    task = get_task(task_id)
    if not task:
        raise HTTPException(404, "Task not found")

    async def _stream():
        queue = task.attach()
        try:
            yield f"data: {orjson.dumps({'type': 'snapshot', **task.snapshot()}).decode()}\n\n"

            final = task.final_event()
            if final:
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                return

            while True:
                try:
                    event = await asyncio.wait_for(queue.get(), timeout=15)
                except asyncio.TimeoutError:
                    yield ": ping\n\n"
                    final = task.final_event()
                    if final:
                        yield f"data: {orjson.dumps(final).decode()}\n\n"
                        return
                    continue

                yield f"data: {orjson.dumps(event).decode()}\n\n"
                if event.get("type") in ("done", "error", "cancelled"):
                    return
        finally:
            task.detach(queue)

    return StreamingResponse(_stream(), media_type="text/event-stream")


@router.post("/{task_id}/cancel")
async def batch_cancel(task_id: str):
    task = get_task(task_id)
    if not task:
        raise HTTPException(404, "Task not found")
    task.cancel()
    return {"status": "success"}
