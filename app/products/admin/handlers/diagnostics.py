"""Admin diagnostics — runtime status + force sync."""

from __future__ import annotations

from fastapi import APIRouter
from fastapi.responses import JSONResponse

router = APIRouter()


@router.get("/status")
async def runtime_status():
    from app.dataplane.account import _directory
    if _directory is None:
        return JSONResponse({"status": "not_initialised"})
    return JSONResponse({
        "status": "ok",
        "size": _directory.size,
        "revision": _directory.revision,
    })


@router.post("/sync")
async def force_sync():
    from app.dataplane.account import _directory
    if _directory is None:
        return JSONResponse({"error": "not_initialised"}, status_code=503)
    changed = await _directory.sync_if_changed()
    return JSONResponse({"changed": changed, "revision": _directory.revision})
