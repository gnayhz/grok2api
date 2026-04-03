"""Shared HTTP session pool for curl_cffi.

Provides a module-level session pool that can be reused across requests,
avoiding the overhead of creating a new session per request.
"""

from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
from typing import AsyncGenerator

from app.platform.logging.logger import logger


class HttpPool:
    """Manages a pool of reusable curl_cffi AsyncSession instances.

    Sessions are created lazily and returned to the pool after use.
    """

    def __init__(self, max_size: int = 8) -> None:
        self._max_size = max_size
        self._pool: asyncio.Queue = asyncio.Queue(maxsize=max_size)
        self._created = 0
        self._lock = asyncio.Lock()

    @asynccontextmanager
    async def acquire(self, **session_kwargs) -> AsyncGenerator:
        """Acquire a session from the pool, yield it, then return it.

        If the pool is empty and under max_size, a new session is created.
        """
        session = await self._get_or_create(**session_kwargs)
        try:
            yield session
        finally:
            await self._return(session)

    async def _get_or_create(self, **session_kwargs):
        # Try to get an existing session without waiting.
        try:
            return self._pool.get_nowait()
        except asyncio.QueueEmpty:
            pass

        async with self._lock:
            if self._created < self._max_size:
                self._created += 1
                return await self._create_session(**session_kwargs)

        # Pool full — wait for a return.
        return await self._pool.get()

    async def _create_session(self, **session_kwargs):
        from curl_cffi.requests import AsyncSession
        return AsyncSession(**session_kwargs)

    async def _return(self, session) -> None:
        try:
            self._pool.put_nowait(session)
        except asyncio.QueueFull:
            # Pool is full, close the extra session.
            try:
                await session.close()
            except Exception:
                pass
            async with self._lock:
                self._created = max(0, self._created - 1)

    async def close_all(self) -> None:
        """Close all sessions in the pool."""
        closed = 0
        while not self._pool.empty():
            try:
                session = self._pool.get_nowait()
                await session.close()
                closed += 1
            except Exception:
                pass
        async with self._lock:
            self._created = 0
        if closed:
            logger.debug("HttpPool: closed {} sessions", closed)


# Module-level default pool.
_default_pool: HttpPool | None = None


def get_http_pool(max_size: int = 8) -> HttpPool:
    """Return the module-level HTTP session pool."""
    global _default_pool
    if _default_pool is None:
        _default_pool = HttpPool(max_size=max_size)
    return _default_pool


__all__ = ["HttpPool", "get_http_pool"]
