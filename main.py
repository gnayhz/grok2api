"""
Grok2API 应用入口

FastAPI 应用初始化和路由注册
"""

from contextlib import asynccontextmanager
import os
import platform
import sys
from pathlib import Path

from dotenv import load_dotenv

BASE_DIR = Path(__file__).resolve().parent
PUBLIC_DIR = BASE_DIR / "_public"

# Ensure the project root is on sys.path (helps when Vercel sets a different CWD)
if str(BASE_DIR) not in sys.path:
    sys.path.insert(0, str(BASE_DIR))

env_file = BASE_DIR / ".env"
if env_file.exists():
    load_dotenv(env_file)

from fastapi import FastAPI, Request  # noqa: E402
from fastapi.middleware.cors import CORSMiddleware  # noqa: E402
from fastapi import Depends  # noqa: E402

from app.core.auth import verify_api_key  # noqa: E402
from app.services.config import config, get_config  # noqa: E402
from app.core.logger import logger, reload_logging_from_config, setup_logging  # noqa: E402
from app.core.exceptions import register_exception_handlers  # noqa: E402
from app.core.response_middleware import ResponseLoggerMiddleware  # noqa: E402
from app.api.v1.chat import router as chat_router  # noqa: E402
from app.api.v1.image import router as image_router  # noqa: E402
from app.api.v1.video import router as video_router  # noqa: E402
from app.api.v1.files import router as files_router  # noqa: E402
from app.api.v1.models import router as models_router  # noqa: E402
from app.api.v1.response import router as responses_router  # noqa: E402
from app.services.account.coordinator import get_account_domain_context  # noqa: E402
from app.services.account.scheduler import get_account_refresh_scheduler  # noqa: E402
from app.services.proxy import get_proxy_refresh_scheduler  # noqa: E402
from app.api.v1.admin import router as admin_router  # noqa: E402
from app.api.v1.function import router as function_router  # noqa: E402
from app.api.pages import router as pages_router  # noqa: E402
from fastapi.responses import RedirectResponse  # noqa: E402
from fastapi.staticfiles import StaticFiles  # noqa: E402

# 初始化日志
setup_logging(
    level=os.getenv("LOG_LEVEL", "INFO"), json_console=False, file_logging=True
)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """应用生命周期管理"""
    # 1. 注册服务默认配置
    from app.services.config import register_defaults
    from app.services.grok.defaults import get_grok_defaults

    register_defaults(get_grok_defaults())

    # 2. 加载配置
    await config.load()
    reload_logging_from_config(
        default_level=os.getenv("LOG_LEVEL", "INFO"),
        json_console=False,
    )

    # 3. 启动服务显示
    logger.info("Starting Grok2API...")
    logger.info(f"Platform: {platform.system()} {platform.release()}")
    logger.info(f"Python: {sys.version.split()[0]}")

    # 4. 初始化 account 域并启动新的刷新调度器
    await get_account_domain_context()
    refresh_enabled = get_config("account.refresh.enabled", True)
    if refresh_enabled:
        account_context = await get_account_domain_context()
        scheduler = get_account_refresh_scheduler(account_context.refresh_service)
        scheduler.start()

    # 5. 启动 proxy 域的 managed clearance 预热调度
    #    环境变量 FLARESOLVERR_URL 会作为初始值写入配置
    _flaresolverr_env = os.getenv("FLARESOLVERR_URL", "")
    if _flaresolverr_env and not get_config("proxy.flaresolverr_url"):
        await config.update({
            "proxy": {
                "enabled": True,
                "flaresolverr_url": _flaresolverr_env,
                "refresh_interval": int(os.getenv("CF_REFRESH_INTERVAL", "600")),
                "timeout": int(os.getenv("CF_TIMEOUT", "60")),
            }
        })

    proxy_scheduler = get_proxy_refresh_scheduler()
    proxy_scheduler.start()

    logger.info("Application startup complete.")
    yield

    # 关闭
    logger.info("Shutting down Grok2API...")

    proxy_scheduler.stop()

    from app.core.storage import StorageFactory

    if StorageFactory._instance:
        await StorageFactory._instance.close()

    if refresh_enabled:
        scheduler = get_account_refresh_scheduler(
            (await get_account_domain_context()).refresh_service
        )
        scheduler.stop()


def create_app() -> FastAPI:
    """创建 FastAPI 应用"""
    app = FastAPI(
        title="Grok2API",
        lifespan=lifespan,
    )

    # CORS 配置
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    # 请求日志和 ID 中间件
    app.add_middleware(ResponseLoggerMiddleware)

    @app.middleware("http")
    async def ensure_config_loaded(request: Request, call_next):
        await config.ensure_loaded()
        return await call_next(request)

    # 注册异常处理器
    register_exception_handlers(app)

    # 注册路由
    app.include_router(
        chat_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        image_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        models_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        responses_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(
        video_router, prefix="/v1", dependencies=[Depends(verify_api_key)]
    )
    app.include_router(files_router, prefix="/v1/files")

    # 静态文件服务（统一使用 /_public/static）
    static_dir = PUBLIC_DIR / "static"
    if static_dir.exists():
        app.mount("/static", StaticFiles(directory=static_dir), name="static")

    # 注册管理与功能玩法路由
    app.include_router(admin_router, prefix="/v1/admin")
    app.include_router(function_router, prefix="/v1/function")
    app.include_router(pages_router)

    @app.get("/favicon.ico", include_in_schema=False)
    def favicon():
        return RedirectResponse(url="/static/common/img/favicon/favicon.ico")
    
    # 健康检查接口（用于 Render、服务器保活检测等）
    @app.get("/health")
    def health():
        """
        健康检查接口，用于服务器保活或 Render 自动检测
        """
        return {"status": "ok"}

    return app    


app = create_app()


if __name__ == "__main__":
    host = os.getenv("SERVER_HOST", "0.0.0.0")
    port = int(os.getenv("SERVER_PORT", "8000"))
    workers = int(os.getenv("SERVER_WORKERS", "1"))
    log_level = os.getenv("LOG_LEVEL", "INFO").lower()
    logger.error(
        "Direct startup via `python main.py` is disabled. "
        "Please run with Granian CLI to avoid Python wrapper issues."
    )
    logger.error(
        "Use: uv run granian --interface asgi "
        f"--host {host} --port {port} --workers {workers} "
        f"--log-level {log_level} main:app"
    )
    raise SystemExit(1)
