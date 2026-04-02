"""
Unified config/lock storage service.

Token persistence has been removed from the core storage layer.
The account domain owns account data; this layer now only handles:
- config load/save
- cross-process locks
- backend lifecycle
"""

from __future__ import annotations

import abc
import asyncio
import os
import time
import tomllib
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any, ClassVar, Dict, Optional

import aiofiles
import orjson

from app.core.logger import logger

try:
    import fcntl
except ImportError:  # pragma: no cover - non-posix platforms
    fcntl = None


DEFAULT_DATA_DIR = Path(__file__).parent.parent.parent / "data"
DATA_DIR = Path(os.getenv("DATA_DIR", str(DEFAULT_DATA_DIR))).expanduser()
CONFIG_FILE = DATA_DIR / "config.toml"
LOCK_DIR = DATA_DIR / ".locks"


def json_dumps(obj: Any) -> str:
    return orjson.dumps(obj).decode("utf-8")


def json_loads(obj: str | bytes) -> Any:
    return orjson.loads(obj)


class StorageError(Exception):
    """Base storage error."""


class BaseStorage(abc.ABC):
    """Config and lock storage contract."""

    @abc.abstractmethod
    async def load_config(self) -> Dict[str, Any] | None:
        """Load config payload."""

    @abc.abstractmethod
    async def save_config(self, data: Dict[str, Any]) -> None:
        """Persist config payload."""

    @abc.abstractmethod
    async def close(self) -> None:
        """Release storage resources."""

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        """Acquire a named lock."""
        yield

    async def verify_connection(self) -> bool:
        return True


class LocalStorage(BaseStorage):
    def __init__(self):
        self._lock = asyncio.Lock()

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        if fcntl is None:
            try:
                async with asyncio.timeout(timeout):
                    async with self._lock:
                        yield
            except asyncio.TimeoutError as error:
                logger.warning("LocalStorage: lock '{}' timed out ({}s)", name, timeout)
                raise StorageError(f"unable to acquire lock '{name}'") from error
            return

        lock_path = LOCK_DIR / f"{name}.lock"
        lock_path.parent.mkdir(parents=True, exist_ok=True)
        fd = None
        locked = False
        start = time.monotonic()

        async with self._lock:
            try:
                fd = open(lock_path, "a+")
                while True:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        locked = True
                        break
                    except BlockingIOError:
                        if time.monotonic() - start >= timeout:
                            raise StorageError(f"unable to acquire lock '{name}'")
                        await asyncio.sleep(0.05)
                yield
            finally:
                if fd:
                    if locked:
                        try:
                            fcntl.flock(fd, fcntl.LOCK_UN)
                        except Exception:
                            pass
                    try:
                        fd.close()
                    except Exception:
                        pass

    async def load_config(self) -> Dict[str, Any] | None:
        if not CONFIG_FILE.exists():
            return {}
        try:
            async with aiofiles.open(CONFIG_FILE, "rb") as file:
                return tomllib.loads((await file.read()).decode("utf-8"))
        except Exception as error:
            logger.error("LocalStorage: failed to load config: {}", error)
            return {}

    async def save_config(self, data: Dict[str, Any]) -> None:
        try:
            lines = []
            for section, items in data.items():
                if not isinstance(items, dict):
                    continue
                lines.append(f"[{section}]")
                for key, value in items.items():
                    if isinstance(value, bool):
                        rendered = "true" if value else "false"
                    elif isinstance(value, str):
                        rendered = json_dumps(value)
                    elif isinstance(value, (int, float)):
                        rendered = str(value)
                    elif isinstance(value, (list, dict)):
                        rendered = json_dumps(value)
                    else:
                        rendered = json_dumps(str(value))
                    lines.append(f'"{key}" = {rendered}')
                lines.append("")

            CONFIG_FILE.parent.mkdir(parents=True, exist_ok=True)
            async with aiofiles.open(CONFIG_FILE, "w", encoding="utf-8") as file:
                await file.write("\n".join(lines))
        except Exception as error:
            logger.error("LocalStorage: failed to save config: {}", error)
            raise StorageError(f"failed to save config: {error}") from error

    async def close(self) -> None:
        return None


class RedisStorage(BaseStorage):
    def __init__(self, url: str):
        try:
            from redis import asyncio as aioredis
        except ImportError as error:
            raise ImportError("redis package is required") from error

        self.redis = aioredis.from_url(
            url,
            decode_responses=True,
            health_check_interval=30,
        )
        self.config_key = "grok2api:config"
        self.lock_prefix = "grok2api:lock:"

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        lock = self.redis.lock(
            f"{self.lock_prefix}{name}",
            timeout=timeout,
            blocking_timeout=5,
        )
        acquired = False
        try:
            acquired = await lock.acquire()
            if not acquired:
                raise StorageError(f"unable to acquire lock '{name}'")
            yield
        finally:
            if acquired:
                try:
                    await lock.release()
                except Exception:
                    pass

    async def verify_connection(self) -> bool:
        try:
            return bool(await self.redis.ping())
        except Exception:
            return False

    async def load_config(self) -> Dict[str, Any] | None:
        try:
            raw_data = await self.redis.hgetall(self.config_key)
            if not raw_data:
                return None

            config: Dict[str, Any] = {}
            for composite_key, value in raw_data.items():
                if "." not in composite_key:
                    continue
                section, key = composite_key.split(".", 1)
                section_payload = config.setdefault(section, {})
                try:
                    section_payload[key] = json_loads(value)
                except Exception:
                    section_payload[key] = value
            return config
        except Exception as error:
            logger.error("RedisStorage: failed to load config: {}", error)
            return None

    async def save_config(self, data: Dict[str, Any]) -> None:
        try:
            mapping = {}
            for section, items in data.items():
                if not isinstance(items, dict):
                    continue
                for key, value in items.items():
                    mapping[f"{section}.{key}"] = json_dumps(value)

            await self.redis.delete(self.config_key)
            if mapping:
                await self.redis.hset(self.config_key, mapping=mapping)
        except Exception as error:
            logger.error("RedisStorage: failed to save config: {}", error)
            raise StorageError(f"failed to save config: {error}") from error

    async def close(self) -> None:
        try:
            await self.redis.close()
        except (RuntimeError, asyncio.CancelledError, Exception):
            pass


class SQLStorage(BaseStorage):
    def __init__(self, url: str, connect_args: dict | None = None):
        try:
            from sqlalchemy.ext.asyncio import async_sessionmaker, create_async_engine
        except ImportError as error:
            raise ImportError("sqlalchemy async support is required") from error

        self.dialect = url.split(":", 1)[0].split("+", 1)[0].lower()
        self.engine = create_async_engine(
            url,
            echo=False,
            pool_size=20,
            max_overflow=10,
            pool_recycle=3600,
            pool_pre_ping=True,
            **({"connect_args": connect_args} if connect_args else {}),
        )
        self.async_session = async_sessionmaker(self.engine, expire_on_commit=False)
        self._initialized = False

    async def _ensure_schema(self) -> None:
        if self._initialized:
            return
        try:
            from sqlalchemy import text

            async with self.engine.begin() as conn:
                await conn.execute(
                    text(
                        """
                        CREATE TABLE IF NOT EXISTS app_config (
                            section VARCHAR(64) NOT NULL,
                            key_name VARCHAR(128) NOT NULL,
                            value TEXT,
                            PRIMARY KEY (section, key_name)
                        )
                        """
                    )
                )
            self._initialized = True
        except Exception as error:
            logger.error("SQLStorage: schema initialization failed: {}", error)
            raise StorageError(f"failed to initialize schema: {error}") from error

    @asynccontextmanager
    async def acquire_lock(self, name: str, timeout: int = 10):
        from sqlalchemy import text
        import hashlib

        await self._ensure_schema()
        lock_name = f"g2a:{hashlib.sha1(name.encode('utf-8')).hexdigest()[:24]}"

        if self.dialect in ("mysql", "mariadb"):
            async with self.async_session() as session:
                result = await session.execute(
                    text("SELECT GET_LOCK(:name, :timeout)"),
                    {"name": lock_name, "timeout": timeout},
                )
                if result.scalar() != 1:
                    raise StorageError(f"unable to acquire lock '{name}'")
                try:
                    yield
                finally:
                    try:
                        await session.execute(
                            text("SELECT RELEASE_LOCK(:name)"),
                            {"name": lock_name},
                        )
                        await session.commit()
                    except Exception:
                        pass
            return

        if self.dialect in ("postgres", "postgresql", "pgsql"):
            lock_key = int.from_bytes(
                hashlib.sha256(name.encode("utf-8")).digest()[:8],
                "big",
                signed=True,
            )
            async with self.async_session() as session:
                start = time.monotonic()
                while True:
                    result = await session.execute(
                        text("SELECT pg_try_advisory_lock(:key)"),
                        {"key": lock_key},
                    )
                    if result.scalar():
                        break
                    if time.monotonic() - start >= timeout:
                        raise StorageError(f"unable to acquire lock '{name}'")
                    await asyncio.sleep(0.1)
                try:
                    yield
                finally:
                    try:
                        await session.execute(
                            text("SELECT pg_advisory_unlock(:key)"),
                            {"key": lock_key},
                        )
                        await session.commit()
                    except Exception:
                        pass
            return

        yield

    async def load_config(self) -> Dict[str, Any] | None:
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                result = await session.execute(
                    text("SELECT section, key_name, value FROM app_config")
                )
                rows = result.fetchall()
                if not rows:
                    return None

                config: Dict[str, Any] = {}
                for section, key, value in rows:
                    section_payload = config.setdefault(section, {})
                    try:
                        section_payload[key] = json_loads(value)
                    except Exception:
                        section_payload[key] = value
                return config
        except Exception as error:
            logger.error("SQLStorage: failed to load config: {}", error)
            return None

    async def save_config(self, data: Dict[str, Any]) -> None:
        await self._ensure_schema()
        from sqlalchemy import text

        try:
            async with self.async_session() as session:
                await session.execute(text("DELETE FROM app_config"))
                params = []
                for section, items in data.items():
                    if not isinstance(items, dict):
                        continue
                    for key, value in items.items():
                        params.append(
                            {
                                "section": section,
                                "key_name": key,
                                "value": json_dumps(value),
                            }
                        )
                if params:
                    await session.execute(
                        text(
                            "INSERT INTO app_config (section, key_name, value) "
                            "VALUES (:section, :key_name, :value)"
                        ),
                        params,
                    )
                await session.commit()
        except Exception as error:
            logger.error("SQLStorage: failed to save config: {}", error)
            raise StorageError(f"failed to save config: {error}") from error

    async def close(self) -> None:
        await self.engine.dispose()


class StorageFactory:
    """Storage backend factory."""

    _instance: Optional[BaseStorage] = None
    _SQL_SSL_PARAM_KEYS = ("sslmode", "ssl-mode", "ssl")
    _PG_SSL_MODE_ALIASES: ClassVar[dict[str, str]] = {
        "disable": "disable",
        "disabled": "disable",
        "false": "disable",
        "0": "disable",
        "no": "disable",
        "off": "disable",
        "prefer": "prefer",
        "preferred": "prefer",
        "allow": "allow",
        "require": "require",
        "required": "require",
        "true": "require",
        "1": "require",
        "yes": "require",
        "on": "require",
        "verify-ca": "verify-ca",
        "verify_ca": "verify-ca",
        "verify-full": "verify-full",
        "verify_full": "verify-full",
        "verify-identity": "verify-full",
        "verify_identity": "verify-full",
    }
    _MY_SSL_MODE_ALIASES: ClassVar[dict[str, str]] = {
        "disable": "disabled",
        "disabled": "disabled",
        "false": "disabled",
        "0": "disabled",
        "no": "disabled",
        "off": "disabled",
        "prefer": "preferred",
        "preferred": "preferred",
        "allow": "preferred",
        "require": "required",
        "required": "required",
        "true": "required",
        "1": "required",
        "yes": "required",
        "on": "required",
        "verify-ca": "verify_ca",
        "verify_ca": "verify_ca",
        "verify-full": "verify_identity",
        "verify_full": "verify_identity",
        "verify-identity": "verify_identity",
        "verify_identity": "verify_identity",
    }

    @classmethod
    def _normalize_ssl_mode(cls, storage_type: str, mode: str) -> str:
        if not mode:
            raise ValueError("SSL mode cannot be empty")

        normalized = mode.strip().lower().replace(" ", "")
        if storage_type == "pgsql":
            canonical = cls._PG_SSL_MODE_ALIASES.get(normalized)
        elif storage_type == "mysql":
            canonical = cls._MY_SSL_MODE_ALIASES.get(normalized)
        else:
            canonical = None

        if not canonical:
            raise ValueError(
                f"Unsupported SSL mode '{mode}' for storage type '{storage_type}'"
            )
        return canonical

    @classmethod
    def _build_mysql_ssl_context(cls, mode: str):
        import ssl as _ssl

        if mode == "disabled":
            return None

        ctx = _ssl.create_default_context()
        if mode in ("preferred", "required"):
            ctx.check_hostname = False
            ctx.verify_mode = _ssl.CERT_NONE
        elif mode == "verify_ca":
            ctx.check_hostname = False
        return ctx

    @classmethod
    def _build_sql_connect_args(
        cls,
        storage_type: str,
        raw_ssl_mode: Optional[str],
    ) -> Optional[dict]:
        if not raw_ssl_mode:
            return None

        mode = cls._normalize_ssl_mode(storage_type, raw_ssl_mode)
        if storage_type == "pgsql":
            return {"ssl": mode}
        if storage_type == "mysql":
            ctx = cls._build_mysql_ssl_context(mode)
            if ctx is None:
                return None
            return {"ssl": ctx}
        return None

    @classmethod
    def _normalize_sql_url(cls, storage_type: str, url: str) -> str:
        if not url or "://" not in url:
            return url
        if storage_type == "mysql":
            if url.startswith("mysql://"):
                return f"mysql+aiomysql://{url[len('mysql://') :]}"
            if url.startswith("mariadb://"):
                return f"mysql+aiomysql://{url[len('mariadb://') :]}"
            if url.startswith("mariadb+aiomysql://"):
                return f"mysql+aiomysql://{url[len('mariadb+aiomysql://') :]}"
            return url
        if storage_type == "pgsql":
            if url.startswith("postgres://"):
                return f"postgresql+asyncpg://{url[len('postgres://') :]}"
            if url.startswith("postgresql://"):
                return f"postgresql+asyncpg://{url[len('postgresql://') :]}"
            if url.startswith("pgsql://"):
                return f"postgresql+asyncpg://{url[len('pgsql://') :]}"
        return url

    @classmethod
    def _prepare_sql_url_and_connect_args(
        cls,
        storage_type: str,
        url: str,
    ) -> tuple[str, Optional[dict]]:
        from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

        normalized_url = cls._normalize_sql_url(storage_type, url)
        if "://" not in normalized_url:
            return normalized_url, None

        parsed = urlparse(normalized_url)
        ssl_mode: Optional[str] = None
        filtered_query_items = []
        ssl_param_keys = {key.lower() for key in cls._SQL_SSL_PARAM_KEYS}

        for key, value in parse_qsl(parsed.query, keep_blank_values=True):
            if key.lower() in ssl_param_keys:
                if ssl_mode is None and value:
                    ssl_mode = value
                continue
            filtered_query_items.append((key, value))

        cleaned_url = urlunparse(
            parsed._replace(query=urlencode(filtered_query_items, doseq=True))
        )
        return cleaned_url, cls._build_sql_connect_args(storage_type, ssl_mode)

    @classmethod
    def get_storage(cls) -> BaseStorage:
        if cls._instance is not None:
            return cls._instance

        storage_type = os.getenv("SERVER_STORAGE_TYPE", "local").lower()
        storage_url = os.getenv("SERVER_STORAGE_URL", "")
        logger.info("StorageFactory: initializing backend '{}'", storage_type)

        if storage_type == "redis":
            if not storage_url:
                raise ValueError("Redis storage requires SERVER_STORAGE_URL")
            cls._instance = RedisStorage(storage_url)
        elif storage_type in ("mysql", "pgsql"):
            if not storage_url:
                raise ValueError("SQL storage requires SERVER_STORAGE_URL")
            normalized_url, connect_args = cls._prepare_sql_url_and_connect_args(
                storage_type,
                storage_url,
            )
            cls._instance = SQLStorage(normalized_url, connect_args=connect_args)
        else:
            cls._instance = LocalStorage()

        return cls._instance


def get_storage() -> BaseStorage:
    return StorageFactory.get_storage()


__all__ = [
    "BaseStorage",
    "CONFIG_FILE",
    "DATA_DIR",
    "DEFAULT_DATA_DIR",
    "LOCK_DIR",
    "LocalStorage",
    "RedisStorage",
    "SQLStorage",
    "StorageError",
    "StorageFactory",
    "get_storage",
    "json_dumps",
    "json_loads",
]
