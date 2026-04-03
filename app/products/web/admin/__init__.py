"""Admin API — router aggregator, shared DI, lightweight endpoints.

All admin endpoints live under ``/admin/api`` with ``verify_admin_key`` guard.
Heavy handlers are split into ``tokens`` and ``batch`` sub-modules.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

import orjson
from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse, Response

from app.platform.auth.middleware import verify_admin_key, get_admin_key
from app.platform.config.snapshot import config, get_config
from app.platform.logging.logger import logger, reload_logging

if TYPE_CHECKING:
    from app.control.account.refresh import AccountRefreshService
    from app.control.account.repository import AccountRepository

# ---------------------------------------------------------------------------
# Shared DI dependencies — inject via Depends, no try/except per call
# ---------------------------------------------------------------------------


def get_repo(request: Request) -> "AccountRepository":
    """Resolve the singleton AccountRepository from app state."""
    return request.app.state.repository


def get_refresh_svc(request: Request) -> "AccountRefreshService":
    """Resolve the singleton AccountRefreshService from app state."""
    return request.app.state.refresh_service


# ---------------------------------------------------------------------------
# Router
# ---------------------------------------------------------------------------

router = APIRouter(prefix="/admin/api", dependencies=[Depends(verify_admin_key)])

# Mount sub-modules
from .tokens import router as _tokens_router  # noqa: E402
from .batch import router as _batch_router    # noqa: E402

router.include_router(_tokens_router)
router.include_router(_batch_router)


# ---------------------------------------------------------------------------
# Lightweight inline endpoints (no separate file needed)
# ---------------------------------------------------------------------------

@router.get("/verify")
async def admin_verify():
    return {"status": "success"}


@router.get("/config")
async def get_config_endpoint():
    return Response(
        content=orjson.dumps(config.raw()),
        media_type="application/json",
    )


@router.post("/config")
async def update_config(data: dict):
    await config.update(data)
    reload_logging()
    return {"status": "success", "message": "配置已更新"}


@router.get("/storage")
async def get_storage_mode():
    backend = get_config("account.storage", "local")
    return {"type": str(backend).strip().lower() or "local"}


@router.get("/status")
async def runtime_status():
    from app.dataplane.account import _directory
    if _directory is None:
        return JSONResponse({"status": "not_initialised"})
    return Response(
        content=orjson.dumps({
            "status": "ok",
            "size": _directory.size,
            "revision": _directory.revision,
        }),
        media_type="application/json",
    )


@router.post("/sync")
async def force_sync():
    from app.dataplane.account import _directory
    if _directory is None:
        return JSONResponse({"error": "not_initialised"}, status_code=503)
    changed = await _directory.sync_if_changed()
    return Response(
        content=orjson.dumps({"changed": changed, "revision": _directory.revision}),
        media_type="application/json",
    )


__all__ = ["router", "get_repo", "get_refresh_svc"]
