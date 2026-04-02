import asyncio
import re

import orjson
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import StreamingResponse

from app.core.auth import get_app_key, verify_app_key
from app.core.batch import create_task, expire_task, get_task
from app.services.config import get_config
from app.core.logger import logger
from app.core.storage import get_storage
from app.services.account.commands import BulkReplacePoolCommand, ListAccountsQuery
from app.services.account.coordinator import (
    get_account_domain_context,
    get_account_management_service,
)
from app.services.account.models import AccountRecord
from app.services.grok.batch_services.nsfw import NSFWService
from app.services.grok.batch_services.usage import UsageService

router = APIRouter()

_TOKEN_CHAR_REPLACEMENTS = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)


def _sanitize_token_text(value) -> str:
    token = "" if value is None else str(value)
    token = token.translate(_TOKEN_CHAR_REPLACEMENTS)
    token = re.sub(r"\s+", "", token)
    if token.startswith("sso="):
        token = token[4:]
    return token.encode("ascii", errors="ignore").decode("ascii")


def _mask_token(token: str) -> str:
    return f"{token[:8]}...{token[-8:]}" if len(token) > 20 else token


def _record_to_token_payload(record: AccountRecord) -> dict:
    return {
        "token": record.token,
        "status": record.status.value,
        "quota": record.quota,
        "consumed": record.consumed,
        "created_at": record.created_at,
        "last_used_at": record.last_used_at,
        "use_count": record.use_count,
        "fail_count": record.fail_count,
        "last_fail_at": record.last_fail_at,
        "last_fail_reason": record.last_fail_reason,
        "last_sync_at": record.last_sync_at,
        "tags": list(record.tags),
        "note": record.note,
        "last_asset_clear_at": record.last_asset_clear_at,
    }


def _normalize_status(value):
    if isinstance(value, str):
        return value.strip().lower()
    return value


def _normalize_token_payload(payload: dict) -> dict | None:
    token_value = _sanitize_token_text(payload.get("token"))
    if not token_value:
        return None

    normalized = dict(payload)
    normalized["token"] = token_value
    normalized["status"] = _normalize_status(normalized.get("status", "active"))
    normalized["quota"] = int(normalized.get("quota", 80) or 0)
    normalized["consumed"] = int(normalized.get("consumed", 0) or 0)
    normalized["use_count"] = int(normalized.get("use_count", 0) or 0)
    normalized["fail_count"] = int(normalized.get("fail_count", 0) or 0)
    raw_tags = normalized.get("tags") or []
    if isinstance(raw_tags, str):
        raw_tags = raw_tags.split(",")
    normalized["tags"] = [str(tag).strip() for tag in raw_tags if str(tag).strip()]
    normalized["note"] = str(normalized.get("note", "") or "")
    return normalized


def _payload_to_token_list(data: dict) -> list[str]:
    tokens: list[str] = []
    if isinstance(data.get("token"), str) and data["token"].strip():
        tokens.append(_sanitize_token_text(data["token"]))
    if isinstance(data.get("tokens"), list):
        tokens.extend(
            _sanitize_token_text(item)
            for item in data["tokens"]
            if _sanitize_token_text(item)
        )
    return list(dict.fromkeys(token for token in tokens if token))


async def _list_all_accounts(*, include_deleted: bool = False) -> list[AccountRecord]:
    service = await get_account_management_service()
    page = 1
    records: list[AccountRecord] = []
    while True:
        result = await service.list_accounts(
            ListAccountsQuery(
                page=page,
                page_size=2000,
                include_deleted=include_deleted,
            )
        )
        records.extend(result.items)
        if page >= result.total_pages:
            break
        page += 1
    return records


@router.get("/tokens", dependencies=[Depends(verify_app_key)])
async def get_tokens():
    """获取所有 Token"""
    accounts = await _list_all_accounts(include_deleted=False)
    results: dict[str, list[dict]] = {}
    for record in accounts:
        results.setdefault(record.pool_name, []).append(_record_to_token_payload(record))
    return {
        "tokens": results,
        "consumed_mode_enabled": get_config(
            "account.runtime.consumed_mode_enabled",
            False,
        ),
    }


@router.post("/tokens", dependencies=[Depends(verify_app_key)])
async def update_tokens(data: dict):
    """全量替换号池到最新 account 格式。"""
    storage = get_storage()
    try:
        service = await get_account_management_service()
        current_accounts = await _list_all_accounts(include_deleted=False)
        current_map: dict[str, dict[str, AccountRecord]] = {}
        for record in current_accounts:
            current_map.setdefault(record.pool_name, {})[record.token] = record

        normalized: dict[str, list] = {}
        for pool_name, tokens in (data or {}).items():
            if not isinstance(tokens, list):
                continue
            pool_items = []
            for item in tokens:
                token_data = {"token": item} if isinstance(item, str) else dict(item or {})
                normalized_item = _normalize_token_payload(token_data)
                if normalized_item is None:
                    logger.warning("Skip empty token in pool '{}'", pool_name)
                    continue

                existing = current_map.get(pool_name, {}).get(normalized_item["token"])
                merged = _record_to_token_payload(existing) if existing else {}
                merged.update(normalized_item)
                if merged.get("tags") is None:
                    merged["tags"] = []
                pool_items.append(merged)
            normalized[pool_name] = pool_items

        async with storage.acquire_lock("account_pool_replace", timeout=10):
            target_pools = set(current_map) | set(normalized)
            for pool_name in target_pools:
                items = [
                    item if isinstance(item, dict) else {"token": item}
                    for item in normalized.get(pool_name, [])
                ]
                command = BulkReplacePoolCommand(
                    pool_name=pool_name,
                    items=[
                        {
                            "token": item.get("token"),
                            "pool_name": pool_name,
                            "status": _normalize_status(item.get("status", "active")),
                            "quota": item.get("quota", 80),
                            "consumed": item.get("consumed", 0),
                            "created_at": item.get("created_at"),
                            "last_used_at": item.get("last_used_at"),
                            "use_count": item.get("use_count", 0),
                            "fail_count": item.get("fail_count", 0),
                            "last_fail_at": item.get("last_fail_at"),
                            "last_fail_reason": item.get("last_fail_reason"),
                            "last_sync_at": item.get("last_sync_at"),
                            "tags": item.get("tags") or [],
                            "note": item.get("note", ""),
                            "last_asset_clear_at": item.get("last_asset_clear_at"),
                        }
                        for item in items
                    ],
                )
                await service.replace_pool(command)

        context = await get_account_domain_context()
        await context.runtime_service.refresh_if_changed()
        return {"status": "success", "message": "Token 已更新"}
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/tokens/refresh", dependencies=[Depends(verify_app_key)])
async def refresh_tokens(data: dict):
    """刷新 Token 状态"""
    try:
        unique_tokens = _payload_to_token_list(data)
        if not unique_tokens:
            raise HTTPException(status_code=400, detail="No tokens provided")

        raw_results = await UsageService.batch(unique_tokens)
        return {
            "status": "success",
            "results": {
                token: bool(res.get("ok")) and res.get("data") is True
                for token, res in raw_results.items()
            },
        }
    except HTTPException:
        raise
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/tokens/refresh/async", dependencies=[Depends(verify_app_key)])
async def refresh_tokens_async(data: dict):
    """刷新 Token 状态（异步批量 + SSE 进度）"""
    unique_tokens = _payload_to_token_list(data)
    if not unique_tokens:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(unique_tokens))

    async def _run():
        try:
            async def _on_item(item: str, res: dict):
                task.record(bool(res.get("ok")) and res.get("data") is True)

            raw_results = await UsageService.batch(
                unique_tokens,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            results: dict[str, bool] = {}
            ok_count = 0
            fail_count = 0
            for token, res in raw_results.items():
                ok = bool(res.get("ok")) and res.get("data") is True
                if ok:
                    ok_count += 1
                else:
                    fail_count += 1
                results[token] = ok

            task.finish(
                {
                    "status": "success",
                    "summary": {
                        "total": len(unique_tokens),
                        "ok": ok_count,
                        "fail": fail_count,
                    },
                    "results": results,
                }
            )
        except Exception as error:
            task.fail_task(str(error))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())
    return {"status": "success", "task_id": task.id, "total": len(unique_tokens)}


@router.get("/batch/{task_id}/stream")
async def batch_stream(task_id: str, request: Request):
    app_key = get_app_key()
    if app_key:
        key = request.query_params.get("app_key")
        if key != app_key:
            raise HTTPException(status_code=401, detail="Invalid authentication token")
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    async def event_stream():
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

    return StreamingResponse(event_stream(), media_type="text/event-stream")


@router.post("/batch/{task_id}/cancel", dependencies=[Depends(verify_app_key)])
async def batch_cancel(task_id: str):
    task = get_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    task.cancel()
    return {"status": "success"}


@router.post("/tokens/nsfw/enable", dependencies=[Depends(verify_app_key)])
async def enable_nsfw(data: dict):
    """批量开启 NSFW (Unhinged) 模式"""
    try:
        unique_tokens = _payload_to_token_list(data)
        if not unique_tokens:
            unique_tokens = [record.token for record in await _list_all_accounts()]
        if not unique_tokens:
            raise HTTPException(status_code=400, detail="No tokens available")

        raw_results = await NSFWService.batch(unique_tokens)
        results = {}
        ok_count = 0
        fail_count = 0
        for token, res in raw_results.items():
            masked = _mask_token(token)
            ok = bool(res.get("ok")) and res.get("data", {}).get("success")
            if ok:
                ok_count += 1
                results[masked] = res.get("data", {})
            else:
                fail_count += 1
                results[masked] = res.get("data") or {"error": res.get("error")}

        return {
            "status": "success",
            "summary": {
                "total": len(unique_tokens),
                "ok": ok_count,
                "fail": fail_count,
            },
            "results": results,
        }
    except HTTPException:
        raise
    except Exception as error:
        logger.error("Enable NSFW failed: {}", error)
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/tokens/nsfw/enable/async", dependencies=[Depends(verify_app_key)])
async def enable_nsfw_async(data: dict):
    """批量开启 NSFW (Unhinged) 模式（异步批量 + SSE 进度）"""
    unique_tokens = _payload_to_token_list(data)
    if not unique_tokens:
        unique_tokens = [record.token for record in await _list_all_accounts()]
    if not unique_tokens:
        raise HTTPException(status_code=400, detail="No tokens available")

    task = create_task(len(unique_tokens))

    async def _run():
        try:
            async def _on_item(item: str, res: dict):
                ok = bool(res.get("ok") and res.get("data", {}).get("success"))
                task.record(ok)

            raw_results = await NSFWService.batch(
                unique_tokens,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            results = {}
            ok_count = 0
            fail_count = 0
            for token, res in raw_results.items():
                masked = _mask_token(token)
                ok = bool(res.get("ok")) and res.get("data", {}).get("success")
                if ok:
                    ok_count += 1
                    results[masked] = res.get("data", {})
                else:
                    fail_count += 1
                    results[masked] = res.get("data") or {"error": res.get("error")}

            task.finish(
                {
                    "status": "success",
                    "summary": {
                        "total": len(unique_tokens),
                        "ok": ok_count,
                        "fail": fail_count,
                    },
                    "results": results,
                }
            )
        except Exception as error:
            task.fail_task(str(error))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())
    return {"status": "success", "task_id": task.id, "total": len(unique_tokens)}
