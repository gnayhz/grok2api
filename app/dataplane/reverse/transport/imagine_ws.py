"""Imagine WebSocket reverse transport.

Connects to wss://grok.com/ws/imagine/listen and streams image events.
Handles blocked-state detection and parallel retry.
"""

from __future__ import annotations

import asyncio
import time
import uuid
from typing import Any, AsyncGenerator

import aiohttp
import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.control.proxy.models import ProxyLease, ProxyScope, RequestKind
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_ws_headers
from app.dataplane.reverse.protocol.xai_image import (
    WS_IMAGINE_URL, build_request_message, classify_image,
)
from .websocket import WebSocketClient


class _BlockedError(Exception):
    """Upstream is reviewing/blocking the image — safe to retry."""


_client = WebSocketClient()


async def stream_images(
    token:        str,
    prompt:       str,
    *,
    aspect_ratio: str  = "2:3",
    n:            int  = 1,
    enable_nsfw:  bool = True,
) -> AsyncGenerator[dict[str, Any], None]:
    """Stream image events from the Imagine WebSocket endpoint.

    Yields dicts with keys: type, image_id, ext, stage, blob, blob_size, url, is_final
    (or type='error' on failure).
    """
    cfg              = get_config()
    timeout_s        = cfg.get_float("image.timeout", 120.0)
    stream_timeout_s = cfg.get_float("image.stream_timeout", 10.0)
    final_timeout_s  = cfg.get_float("image.final_timeout", 30.0)
    blocked_grace_s  = min(max(cfg.get_float("image.blocked_grace_seconds", 10.0), 1.0), final_timeout_s)
    final_min_bytes  = cfg.get_int("image.final_min_bytes", 50_000)
    medium_min_bytes = cfg.get_int("image.medium_min_bytes", 5_000)
    max_retries      = max(1, cfg.get_int("image.max_retries", 1))
    parallel_ok      = cfg.get_bool("image.blocked_parallel_enabled", True)

    async def _once() -> list[dict[str, Any]]:
        items: list[dict[str, Any]] = []
        async for ev in _stream_once(
            token, prompt,
            aspect_ratio     = aspect_ratio,
            n                = n,
            enable_nsfw      = enable_nsfw,
            timeout_s        = timeout_s,
            stream_timeout_s = stream_timeout_s,
            final_timeout_s  = final_timeout_s,
            blocked_grace_s  = blocked_grace_s,
            final_min_bytes  = final_min_bytes,
            medium_min_bytes = medium_min_bytes,
        ):
            items.append(ev)
        return items

    for attempt in range(max_retries):
        try:
            items = await _once()
            for ev in items:
                yield ev
            return
        except _BlockedError:
            remaining = max_retries - attempt - 1
            if remaining > 0 and parallel_ok:
                logger.warning("Imagine blocked; launching {} parallel retries", remaining)
                tasks = [asyncio.create_task(_once()) for _ in range(remaining)]
                results = await asyncio.gather(*tasks, return_exceptions=True)
                for result in results:
                    if isinstance(result, Exception):
                        continue
                    if any(ev.get("is_final") for ev in result if isinstance(ev, dict)):
                        for ev in result:
                            yield ev
                        return
                yield {"type": "error", "error_code": "blocked", "error": "blocked_no_final_image"}
                return
            if attempt + 1 < max_retries:
                logger.warning("Imagine blocked, retry {}/{}", attempt + 1, max_retries)
                continue
            yield {"type": "error", "error_code": "blocked", "error": "blocked_no_final_image"}
            return
        except Exception as exc:
            logger.error("Imagine stream error: {}", exc)
            yield {"type": "error", "error_code": "stream_error", "error": str(exc)}
            return


async def _stream_once(
    token:        str,
    prompt:       str,
    *,
    aspect_ratio:     str,
    n:                int,
    enable_nsfw:      bool,
    timeout_s:        float,
    stream_timeout_s: float,
    final_timeout_s:  float,
    blocked_grace_s:  float,
    final_min_bytes:  int,
    medium_min_bytes: int,
) -> AsyncGenerator[dict[str, Any], None]:
    request_id = str(uuid.uuid4())
    proxy      = await get_proxy_runtime()
    lease      = await proxy.acquire(
        scope = ProxyScope.APP,
        kind  = RequestKind.WEBSOCKET,
    )
    headers = build_ws_headers(token=token, lease=lease)

    try:
        conn = await _client.connect(
            WS_IMAGINE_URL,
            headers   = headers,
            timeout   = timeout_s,
            ws_kwargs = {"heartbeat": 20, "receive_timeout": stream_timeout_s},
            lease     = lease,
        )
    except Exception as exc:
        status = getattr(exc, "status", None)
        logger.error("Imagine WebSocket connect failed: {}", exc)
        yield {
            "type":       "error",
            "error_code": "rate_limit_exceeded" if status == 429 else "connection_failed",
            "error":      str(exc),
        }
        return

    try:
        async with conn as ws:
            await ws.send_json(build_request_message(request_id, prompt, aspect_ratio, enable_nsfw))
            logger.info("Imagine request sent: prompt={!r:.50} ratio={}", prompt, aspect_ratio)

            final_ids:            set[str] = set()
            completed:            int       = 0
            start                           = time.monotonic()
            last_activity                   = start
            medium_received_at:   float | None = None

            while time.monotonic() - start < timeout_s:
                try:
                    ws_msg = await asyncio.wait_for(ws.receive(), timeout=5.0)
                except asyncio.TimeoutError:
                    now = time.monotonic()
                    if medium_received_at and completed == 0 and now - medium_received_at > blocked_grace_s:
                        logger.warning("Imagine blocked: medium received but no final in {:.1f}s", blocked_grace_s)
                        raise _BlockedError()
                    if completed > 0 and now - last_activity > 10.0:
                        break
                    continue

                if ws_msg.type == aiohttp.WSMsgType.TEXT:
                    last_activity = time.monotonic()
                    try:
                        msg = orjson.loads(ws_msg.data)
                    except Exception:
                        continue

                    msg_type = msg.get("type")
                    if msg_type == "image":
                        info = classify_image(
                            msg.get("url", ""), msg.get("blob", ""),
                            final_min_bytes  = final_min_bytes,
                            medium_min_bytes = medium_min_bytes,
                        )
                        if not info:
                            continue
                        if info["stage"] == "medium" and medium_received_at is None:
                            medium_received_at = time.monotonic()
                        if info["is_final"] and info["image_id"] not in final_ids:
                            final_ids.add(info["image_id"])
                            completed += 1
                        yield info

                    elif msg_type == "error":
                        logger.warning("Imagine WS error: {} {}", msg.get("err_code"), msg.get("err_msg"))
                        yield {
                            "type":       "error",
                            "error_code": msg.get("err_code", ""),
                            "error":      msg.get("err_msg", ""),
                        }
                        return

                    if completed >= n:
                        break

                    if medium_received_at and completed == 0 and time.monotonic() - medium_received_at > final_timeout_s:
                        logger.warning("Imagine final-timeout: no final image in {:.1f}s", final_timeout_s)
                        raise _BlockedError()

                elif ws_msg.type in (aiohttp.WSMsgType.CLOSED, aiohttp.WSMsgType.ERROR):
                    yield {"type": "error", "error_code": "ws_closed", "error": str(ws_msg.type)}
                    return

    except aiohttp.ClientError as exc:
        yield {"type": "error", "error_code": "connection_failed", "error": str(exc)}


__all__ = ["stream_images"]
