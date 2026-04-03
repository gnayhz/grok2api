"""Static HTML page serving for admin and webui."""

from __future__ import annotations

from pathlib import Path

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from app.platform.auth.middleware import is_function_enabled

router = APIRouter(include_in_schema=False)

STATIC_DIR = Path(__file__).resolve().parents[2] / "assets"


def _serve(relative_path: str) -> FileResponse:
    file_path = STATIC_DIR / relative_path
    if not file_path.exists():
        raise HTTPException(status_code=404, detail="Page not found")
    return FileResponse(file_path)


# ---------------------------------------------------------------------------
# Admin pages
# ---------------------------------------------------------------------------

@router.get("/admin")
async def admin_root():
    return RedirectResponse(url="/admin/login")


@router.get("/admin/login")
async def admin_login():
    return _serve("admin/pages/login.html")


@router.get("/admin/config")
async def admin_config_page():
    return _serve("admin/pages/config.html")


@router.get("/admin/cache")
async def admin_cache_page():
    return _serve("admin/pages/cache.html")


@router.get("/admin/token")
async def admin_token_page():
    return _serve("admin/pages/token.html")


# ---------------------------------------------------------------------------
# WebUI pages
# ---------------------------------------------------------------------------

@router.get("/")
async def root():
    if is_function_enabled():
        return RedirectResponse(url="/login")
    return RedirectResponse(url="/admin/login")


@router.get("/login")
async def webui_login():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve("function/pages/login.html")


@router.get("/imagine")
async def webui_imagine():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve("function/pages/imagine.html")


@router.get("/voice")
async def webui_voice():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve("function/pages/voice.html")


@router.get("/video")
async def webui_video():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve("function/pages/video.html")


@router.get("/chat")
async def webui_chat():
    if not is_function_enabled():
        raise HTTPException(status_code=404, detail="Not Found")
    return _serve("function/pages/chat.html")
