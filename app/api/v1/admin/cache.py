import asyncio
from typing import List

from fastapi import APIRouter, Depends, HTTPException, Query, Request

from app.core.auth import verify_app_key
from app.core.batch import create_task, expire_task
from app.services.account.commands import ListAccountsQuery
from app.services.account.coordinator import get_account_management_service
from app.services.account.models import AccountRecord
from app.services.grok.batch_services.assets import DeleteService, ListService

router = APIRouter()


def _mask_token(token: str) -> str:
    return f"{token[:8]}...{token[-16:]}" if len(token) > 24 else token


async def _list_all_accounts() -> list[AccountRecord]:
    service = await get_account_management_service()
    page = 1
    records: list[AccountRecord] = []
    while True:
        result = await service.list_accounts(
            ListAccountsQuery(
                page=page,
                page_size=2000,
                include_deleted=False,
            )
        )
        records.extend(result.items)
        if page >= result.total_pages:
            break
        page += 1
    return records


async def _build_online_accounts() -> list[dict]:
    accounts = await _list_all_accounts()
    return [
        {
            "token": record.token,
            "token_masked": _mask_token(record.token),
            "pool": record.pool_name,
            "status": record.status.value,
            "last_asset_clear_at": record.last_asset_clear_at,
        }
        for record in accounts
    ]


def _query_tokens(value: str | None) -> list[str]:
    if not value:
        return []
    return [token.strip() for token in value.split(",") if token.strip()]


def _detail_from_error(token: str, account_map: dict, error: str) -> dict:
    account = account_map.get(token)
    return {
        "token": token,
        "token_masked": account["token_masked"] if account else token,
        "count": 0,
        "status": f"error: {error}",
        "last_asset_clear_at": account["last_asset_clear_at"] if account else None,
    }


@router.get("/cache", dependencies=[Depends(verify_app_key)])
async def cache_stats(request: Request):
    """获取缓存统计"""
    from app.services.grok.utils.cache import CacheService

    try:
        cache_service = CacheService()
        image_stats = cache_service.get_stats("image")
        video_stats = cache_service.get_stats("video")

        accounts = await _build_online_accounts()
        account_map = {account["token"]: account for account in accounts}
        scope = request.query_params.get("scope")
        selected_token = request.query_params.get("token")
        selected_tokens = _query_tokens(request.query_params.get("tokens"))

        online_stats = {
            "count": 0,
            "status": "unknown",
            "token": None,
            "last_asset_clear_at": None,
        }
        online_details = []

        if selected_tokens:
            total = 0
            raw_results = await ListService.fetch_assets_details(selected_tokens, account_map)
            for token, res in raw_results.items():
                if res.get("ok"):
                    data = res.get("data", {})
                    detail = data.get("detail")
                    total += data.get("count", 0)
                else:
                    detail = _detail_from_error(token, account_map, str(res.get("error")))
                if detail:
                    online_details.append(detail)
            online_stats = {
                "count": total,
                "status": "ok" if selected_tokens else "no_token",
                "token": None,
                "last_asset_clear_at": None,
            }
            scope = "selected"
        elif scope == "all":
            total = 0
            tokens = list(dict.fromkeys(account["token"] for account in accounts))
            raw_results = await ListService.fetch_assets_details(tokens, account_map)
            for token, res in raw_results.items():
                if res.get("ok"):
                    data = res.get("data", {})
                    detail = data.get("detail")
                    total += data.get("count", 0)
                else:
                    detail = _detail_from_error(token, account_map, str(res.get("error")))
                if detail:
                    online_details.append(detail)
            online_stats = {
                "count": total,
                "status": "ok" if accounts else "no_token",
                "token": None,
                "last_asset_clear_at": None,
            }
        elif selected_token:
            raw_results = await ListService.fetch_assets_details([selected_token], account_map)
            res = raw_results.get(selected_token, {})
            data = res.get("data", {})
            detail = data.get("detail") if res.get("ok") else None
            if detail:
                online_stats = {
                    "count": data.get("count", 0),
                    "status": detail.get("status", "ok"),
                    "token": detail.get("token"),
                    "token_masked": detail.get("token_masked"),
                    "last_asset_clear_at": detail.get("last_asset_clear_at"),
                }
            else:
                match = account_map.get(selected_token)
                online_stats = {
                    "count": 0,
                    "status": f"error: {res.get('error')}",
                    "token": selected_token,
                    "token_masked": match["token_masked"] if match else selected_token,
                    "last_asset_clear_at": match["last_asset_clear_at"] if match else None,
                }
        else:
            online_stats = {
                "count": 0,
                "status": "not_loaded",
                "token": None,
                "last_asset_clear_at": None,
            }

        return {
            "local_image": image_stats,
            "local_video": video_stats,
            "online": online_stats,
            "online_accounts": accounts,
            "online_scope": scope or "none",
            "online_details": online_details,
        }
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.get("/cache/list", dependencies=[Depends(verify_app_key)])
async def list_local(
    cache_type: str = "image",
    type_: str = Query(default=None, alias="type"),
    page: int = 1,
    page_size: int = 1000,
):
    """列出本地缓存文件"""
    from app.services.grok.utils.cache import CacheService

    try:
        if type_:
            cache_type = type_
        cache_service = CacheService()
        result = cache_service.list_files(cache_type, page, page_size)
        return {"status": "success", **result}
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/cache/clear", dependencies=[Depends(verify_app_key)])
async def clear_local(data: dict):
    """清理本地缓存"""
    from app.services.grok.utils.cache import CacheService

    cache_type = data.get("type", "image")

    try:
        cache_service = CacheService()
        result = cache_service.clear(cache_type)
        return {"status": "success", "result": result}
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/cache/item/delete", dependencies=[Depends(verify_app_key)])
async def delete_local_item(data: dict):
    """删除单个本地缓存文件"""
    from app.services.grok.utils.cache import CacheService

    cache_type = data.get("type", "image")
    name = data.get("name")
    if not name:
        raise HTTPException(status_code=400, detail="Missing file name")
    try:
        cache_service = CacheService()
        result = cache_service.delete_file(cache_type, name)
        return {"status": "success", "result": result}
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/cache/online/clear", dependencies=[Depends(verify_app_key)])
async def clear_online(data: dict):
    """清理在线缓存"""
    try:
        tokens = data.get("tokens")
        if isinstance(tokens, list):
            token_list = [token.strip() for token in tokens if isinstance(token, str) and token.strip()]
            if not token_list:
                raise HTTPException(status_code=400, detail="No tokens provided")

            raw_results = await DeleteService.clear_assets(list(dict.fromkeys(token_list)))
            results = {}
            for token, res in raw_results.items():
                if res.get("ok"):
                    results[token] = res.get("data", {})
                else:
                    results[token] = {"status": "error", "error": res.get("error")}
            return {"status": "success", "results": results}

        token = data.get("token")
        if not token:
            accounts = await _list_all_accounts()
            token = accounts[0].token if accounts else None
        if not token:
            raise HTTPException(
                status_code=400, detail="No available token to perform cleanup"
            )

        raw_results = await DeleteService.clear_assets([token])
        res = raw_results.get(token, {})
        payload = res.get("data", {})
        if res.get("ok") and payload.get("status") == "success":
            return {"status": "success", "result": payload.get("result")}
        return {"status": "error", "error": payload.get("error") or res.get("error")}
    except HTTPException:
        raise
    except Exception as error:
        raise HTTPException(status_code=500, detail=str(error))


@router.post("/cache/online/clear/async", dependencies=[Depends(verify_app_key)])
async def clear_online_async(data: dict):
    """清理在线缓存（异步批量 + SSE 进度）"""
    tokens = data.get("tokens")
    if not isinstance(tokens, list):
        raise HTTPException(status_code=400, detail="No tokens provided")

    token_list = [token.strip() for token in tokens if isinstance(token, str) and token.strip()]
    if not token_list:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(token_list))

    async def _run():
        try:
            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await DeleteService.clear_assets(
                token_list,
                include_ok=True,
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
                payload = res.get("data", {})
                if payload.get("ok"):
                    ok_count += 1
                    results[token] = {"status": "success", "result": payload.get("result")}
                else:
                    fail_count += 1
                    results[token] = {"status": "error", "error": payload.get("error")}

            task.finish(
                {
                    "status": "success",
                    "summary": {
                        "total": len(token_list),
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
    return {"status": "success", "task_id": task.id, "total": len(token_list)}


@router.post("/cache/online/load/async", dependencies=[Depends(verify_app_key)])
async def load_cache_async(data: dict):
    """在线资产统计（异步批量 + SSE 进度）"""
    from app.services.grok.utils.cache import CacheService

    accounts = await _build_online_accounts()
    account_map = {account["token"]: account for account in accounts}

    tokens = data.get("tokens")
    scope = data.get("scope")
    selected_tokens: List[str] = []
    if isinstance(tokens, list):
        selected_tokens = [str(token).strip() for token in tokens if str(token).strip()]

    if not selected_tokens and scope == "all":
        selected_tokens = [account["token"] for account in accounts]
        scope = "all"
    elif selected_tokens:
        scope = "selected"
    else:
        raise HTTPException(status_code=400, detail="No tokens provided")

    task = create_task(len(selected_tokens))

    async def _run():
        try:
            cache_service = CacheService()
            image_stats = cache_service.get_stats("image")
            video_stats = cache_service.get_stats("video")

            async def _on_item(item: str, res: dict):
                ok = bool(res.get("data", {}).get("ok"))
                task.record(ok)

            raw_results = await ListService.fetch_assets_details(
                selected_tokens,
                account_map,
                include_ok=True,
                on_item=_on_item,
                should_cancel=lambda: task.cancelled,
            )

            if task.cancelled:
                task.finish_cancelled()
                return

            online_details = []
            total = 0
            for token, res in raw_results.items():
                payload = res.get("data", {})
                detail = payload.get("detail")
                if detail:
                    online_details.append(detail)
                total += payload.get("count", 0)

            task.finish(
                {
                    "local_image": image_stats,
                    "local_video": video_stats,
                    "online": {
                        "count": total,
                        "status": "ok" if selected_tokens else "no_token",
                        "token": None,
                        "last_asset_clear_at": None,
                    },
                    "online_accounts": accounts,
                    "online_scope": scope or "none",
                    "online_details": online_details,
                }
            )
        except Exception as error:
            task.fail_task(str(error))
        finally:
            asyncio.create_task(expire_task(task.id, 300))

    asyncio.create_task(_run())
    return {"status": "success", "task_id": task.id, "total": len(selected_tokens)}
