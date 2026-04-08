"""Web product — unified pages + API for the statics-based frontend."""

from pathlib import Path

from fastapi import APIRouter, Depends, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from app.platform.auth.middleware import is_webui_enabled, verify_webui_key
from app.platform.meta import get_project_version
from app.platform.update_check import get_latest_release_info
from .admin import router as admin_api_router
from .webui import router as webui_router

router = APIRouter()

# Mount admin API sub-router (/admin/api/*)
router.include_router(admin_api_router)
router.include_router(webui_router)

_DIR = Path(__file__).resolve().parents[2] / "statics"


def _serve(path: str) -> FileResponse:
    f = _DIR / path
    if not f.exists():
        raise HTTPException(404, "Page not found")
    return FileResponse(f)


# --- Admin pages ---
@router.get("/admin", include_in_schema=False)
async def admin_root():
    return RedirectResponse("/admin/login")

@router.get("/admin/login", include_in_schema=False)
async def admin_login():
    return _serve("admin/login.html")

@router.get("/admin/account", include_in_schema=False)
async def admin_account():
    return _serve("admin/account.html")

@router.get("/admin/config", include_in_schema=False)
async def admin_config():
    return _serve("admin/config.html")

@router.get("/admin/cache", include_in_schema=False)
async def admin_cache():
    return _serve("admin/cache.html")


# --- WebUI ---
@router.get("/webui", include_in_schema=False)
async def webui_root():
    return RedirectResponse("/webui/login")

@router.get("/webui/login", include_in_schema=False)
async def webui_login():
    if not is_webui_enabled():
        raise HTTPException(404, "Not Found")
    return _serve("webui/login.html")

@router.get("/webui/api/verify", dependencies=[Depends(verify_webui_key)])
async def webui_verify():
    return {"status": "ok"}


@router.get("/meta", include_in_schema=False)
async def app_meta():
    return {"version": get_project_version()}


@router.get("/meta/update", include_in_schema=False)
async def app_update_meta():
    return await get_latest_release_info()
