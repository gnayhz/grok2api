"""Grok2API application entry point.

Start with:
  uv run granian --interface asgi --host 0.0.0.0 --port 8000 --workers 1 app.main:app
"""

import os
import platform
import sys
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from fastapi.staticfiles import StaticFiles

from app.platform.logging.logger import logger, setup_logging, reload_logging
from app.platform.config.snapshot import config as _config
from app.platform.errors import AppError
from app.platform.meta import get_project_version

# ---------------------------------------------------------------------------
# Early logging setup (before config is loaded)
# ---------------------------------------------------------------------------

setup_logging(level=os.getenv("LOG_LEVEL", "INFO"))


# ---------------------------------------------------------------------------
# Lifespan
# ---------------------------------------------------------------------------

@asynccontextmanager
async def lifespan(app: FastAPI):
    # 1. Load configuration.
    await _config.load()
    reload_logging()
    logger.info(
        "Grok2API starting: python={} platform={}",
        sys.version.split()[0],
        platform.system(),
    )

    # 2. Initialise account repository and bootstrap runtime table.
    from app.control.account.backends.factory import create_repository, describe_repository_target
    from app.control.account.runtime import set_refresh_service
    from app.control.account.scheduler import get_account_refresh_scheduler
    from app.dataplane.account import get_account_directory

    storage_backend, storage_target = describe_repository_target()
    logger.info("Account storage: backend={} target={}", storage_backend, storage_target)

    repo      = create_repository()
    await repo.initialize()
    directory = await get_account_directory(repo)

    # Expose repository on app.state for admin handlers.
    app.state.repository = repo
    app.state.directory  = directory

    # 3. Start account refresh scheduler.
    from app.control.account.refresh import AccountRefreshService
    refresh_svc = AccountRefreshService(repo)
    scheduler   = get_account_refresh_scheduler(refresh_svc)
    scheduler.start()

    # Expose refresh service for fire-and-forget post-call quota sync.
    app.state.refresh_service = refresh_svc
    set_refresh_service(refresh_svc)

    # 4. Initialise proxy directory.
    from app.control.proxy import get_proxy_directory
    await get_proxy_directory()

    logger.info("Application startup complete.")
    yield

    # -----------
    # Shutdown
    # -----------
    logger.info("Grok2API shutting down.")
    scheduler.stop()
    set_refresh_service(None)
    await repo.close()


# ---------------------------------------------------------------------------
# Application factory
# ---------------------------------------------------------------------------

def create_app() -> FastAPI:
    app = FastAPI(
        title       = "Grok2API",
        version     = get_project_version(),
        description = "OpenAI-compatible API gateway for Grok",
        lifespan    = lifespan,
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins     = ["*"],
        allow_credentials = True,
        allow_methods     = ["*"],
        allow_headers     = ["*"],
    )

    # Ensure config is loaded on every request.
    @app.middleware("http")
    async def _ensure_config(request: Request, call_next):
        await _config.load()
        return await call_next(request)

    # Global exception handler — converts AppError to JSON.
    @app.exception_handler(AppError)
    async def _app_error_handler(request: Request, exc: AppError):
        return JSONResponse(exc.to_dict(), status_code=exc.status)

    @app.exception_handler(Exception)
    async def _generic_error_handler(request: Request, exc: Exception):
        logger.exception("Unhandled exception: {}", exc)
        return JSONResponse(
            {"error": {"message": "Internal server error", "type": "server_error"}},
            status_code=500,
        )

    # Routers.
    from app.products.web           import router as web_router
    from app.products.openai.router import router as openai_router

    app.include_router(web_router)
    app.include_router(openai_router)

    # Static assets — new statics only.
    _statics_dir = Path(__file__).resolve().parent / "statics"
    if _statics_dir.is_dir():
        app.mount("/static", StaticFiles(directory=_statics_dir), name="static")

    @app.get("/favicon.ico", include_in_schema=False)
    def favicon():
        from fastapi.responses import FileResponse as _FR
        _ico = _statics_dir / "favicon.ico"
        if _ico.exists():
            return _FR(_ico)
        return JSONResponse({"error": "not found"}, status_code=404)

    @app.get("/health", include_in_schema=False)
    def health():
        return {"status": "ok"}

    return app


app = create_app()


if __name__ == "__main__":
    logger.error(
        "Direct startup disabled. Use: "
        "uv run granian --interface asgi --host 0.0.0.0 --port 8000 app.main:app"
    )
    raise SystemExit(1)
