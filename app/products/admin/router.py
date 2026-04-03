"""Admin API router — slim aggregator for all admin sub-routers."""

from __future__ import annotations

from fastapi import APIRouter, Depends

from app.platform.auth.middleware import verify_admin_key
from .handlers.config import router as config_router
from .handlers.tokens import router as tokens_router
from .handlers.diagnostics import router as diagnostics_router
from .handlers.batch_ops import router as batch_ops_router
from .handlers.batch_stream import router as batch_stream_router
from .handlers.cache import router as cache_router

router = APIRouter(prefix="/admin/api", dependencies=[Depends(verify_admin_key)])
router.include_router(config_router)
router.include_router(tokens_router)
router.include_router(diagnostics_router)
router.include_router(batch_ops_router)
router.include_router(batch_stream_router)
router.include_router(cache_router)

__all__ = ["router"]
