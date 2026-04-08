"""Admin online-asset management — list, delete, clear per token."""

import asyncio
from typing import TYPE_CHECKING

import orjson
from fastapi import APIRouter, Depends
from fastapi.responses import Response
from pydantic import BaseModel

from app.platform.errors import UpstreamError
from app.control.account.commands import ListAccountsQuery

if TYPE_CHECKING:
    from app.control.account.repository import AccountRepository

from . import get_repo

router = APIRouter(prefix="/assets")


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


async def _fetch_one(token: str) -> dict:
    from app.dataplane.reverse.transport.assets import list_assets
    try:
        resp  = await list_assets(token)
        items = resp.get("assets", resp.get("items", []))
        return {
            "token":  token,
            "masked": _mask(token),
            "count":  len(items),
            "assets": [
                {
                    "id":           item.get("id") or item.get("assetId") or "",
                    "name":         item.get("fileName") or item.get("name") or "",
                    "file_path":    item.get("filePath") or item.get("file_path") or "",
                    "content_type": item.get("contentType") or item.get("content_type") or "",
                    "size":         item.get("fileSize") or item.get("size") or 0,
                    "created_at":   item.get("createdAt") or item.get("created_at") or "",
                }
                for item in items
            ],
            "error": None,
        }
    except Exception as exc:
        return {
            "token":  token,
            "masked": _mask(token),
            "count":  0,
            "assets": [],
            "error":  str(exc),
        }


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------

class DeleteItemRequest(BaseModel):
    token:    str
    asset_id: str


class ClearTokenRequest(BaseModel):
    token: str


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.get("")
async def list_all_assets(repo: "AccountRepository" = Depends(get_repo)):
    """Fetch asset lists for all tokens concurrently."""
    tokens = await _list_all_tokens(repo)
    if not tokens:
        return Response(
            content=orjson.dumps({"tokens": [], "total_assets": 0}),
            media_type="application/json",
        )

    # Concurrency is governed by the global list_assets semaphore in the transport layer.
    results = await asyncio.gather(*[_fetch_one(t) for t in tokens])
    total   = sum(r["count"] for r in results)

    return Response(
        content=orjson.dumps({"tokens": list(results), "total_assets": total}),
        media_type="application/json",
    )


@router.post("/delete-item")
async def delete_item(req: DeleteItemRequest):
    """Delete a single asset by token + asset_id."""
    from app.dataplane.reverse.transport.assets import delete_asset
    try:
        await delete_asset(req.token, req.asset_id)
        return {"status": "success"}
    except Exception as exc:
        raise UpstreamError(str(exc)) from exc


@router.post("/clear-token")
async def clear_token_assets(req: ClearTokenRequest):
    """Delete all assets for one token concurrently."""
    from app.dataplane.reverse.transport.assets import list_assets, delete_asset
    try:
        resp  = await list_assets(req.token)
        items = resp.get("assets", resp.get("items", []))

        async def _del(item: dict) -> int:
            aid = item.get("id") or item.get("assetId")
            if not aid:
                return 0
            await delete_asset(req.token, aid)
            return 1

        results = await asyncio.gather(*[_del(i) for i in items], return_exceptions=True)
        deleted = sum(r for r in results if isinstance(r, int))
        return {"status": "success", "deleted": deleted}
    except Exception as exc:
        raise UpstreamError(str(exc)) from exc


__all__ = ["router"]
