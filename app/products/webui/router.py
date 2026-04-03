"""WebUI router — slim aggregator for function + static endpoints."""

from __future__ import annotations

from fastapi import APIRouter, Depends

from app.platform.auth.middleware import verify_function_key
from .voice import router as voice_router
from .imagine import router as imagine_router
from .pages import router as pages_router
from .files import router as files_router

router = APIRouter()
router.include_router(voice_router, prefix="/function", dependencies=[Depends(verify_function_key)])
router.include_router(imagine_router, prefix="/function", dependencies=[Depends(verify_function_key)])
router.include_router(pages_router)    # no prefix — serves at root
router.include_router(files_router)    # no prefix — serves at root

__all__ = ["router"]
