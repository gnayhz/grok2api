"""Image generation service — Imagine WebSocket + OpenAI format output."""

from __future__ import annotations

import asyncio
import base64
import time
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.control.model.registry import resolve as resolve_model
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.transport.imagine_ws import stream_images
from .response import make_response_id, make_stream_chunk, build_usage

# Aspect-ratio alias table (size string or ratio string → canonical ratio).
_RATIO_MAP: dict[str, str] = {
    "1280x720":  "16:9", "16:9": "16:9",
    "720x1280":  "9:16", "9:16": "9:16",
    "1792x1024": "3:2",  "3:2":  "3:2",
    "1024x1792": "2:3",  "2:3":  "2:3",
    "1024x1024": "1:1",  "1:1":  "1:1",
}


def resolve_aspect_ratio(size: str) -> str:
    return _RATIO_MAP.get(size, "2:3")


def _wrap_image_markdown(content: str, response_format: str) -> str:
    if not content:
        return ""
    if response_format == "url":
        return f"![image]({content})"
    return f"![image](data:image/png;base64,{content})"


async def generate(
    *,
    model:           str,
    prompt:          str,
    n:               int  = 1,
    size:            str  = "1024x1024",
    response_format: str  = "url",
    stream:          bool = False,
    chat_format:     bool = False,
) -> dict | AsyncGenerator[str, None]:
    """Generate images via the Imagine WebSocket reverse.

    Returns:
      - Non-streaming: OpenAI images.generations response dict, or chat response if chat_format=True.
      - Streaming: async generator of SSE strings (chat.completion.chunk format).
    """
    cfg          = get_config()
    spec         = resolve_model(model)
    aspect_ratio = resolve_aspect_ratio(size)
    enable_nsfw  = cfg.get_bool("app.enable_nsfw", True)

    # Acquire account.
    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    ts   = now_s()
    acct = await _acct_dir.reserve(
        pool_id        = spec.pool_id(),
        mode_id        = int(spec.mode_id),
        now_s_override = ts,
    )
    if acct is None:
        raise RateLimitError("No available accounts for image generation")

    token = acct.token

    async def _collect() -> list[str]:
        """Collect n final images and return as list of URL/base64 strings."""
        finals: list[str] = []
        async for ev in stream_images(
            token,
            prompt,
            aspect_ratio = aspect_ratio,
            n            = n,
            enable_nsfw  = enable_nsfw,
        ):
            if ev.get("type") == "error":
                logger.warning("Image generation error: {} {}", ev.get("error_code"), ev.get("error"))
                raise UpstreamError(f"Image generation failed: {ev.get('error', 'unknown')}")
            if ev.get("is_final"):
                blob = ev.get("blob", "")
                if response_format in ("b64_json", "base64"):
                    finals.append(blob)
                else:
                    finals.append(ev.get("url", ""))
                if len(finals) >= n:
                    break
        return finals

    response_id = make_response_id()

    if stream:
        # Stream each final image as a chat chunk.
        async def _sse_stream() -> AsyncGenerator[str, None]:
            success = False
            try:
                async for ev in stream_images(
                    token, prompt,
                    aspect_ratio = aspect_ratio,
                    n            = n,
                    enable_nsfw  = enable_nsfw,
                ):
                    if ev.get("type") == "error":
                        raise UpstreamError(f"Image error: {ev.get('error', '')}")
                    if not ev.get("is_final"):
                        continue
                    blob = ev.get("blob", "")
                    content = (
                        blob if response_format in ("b64_json", "base64")
                        else ev.get("url", "")
                    )
                    if chat_format:
                        content = _wrap_image_markdown(content, response_format)
                    chunk = make_stream_chunk(response_id, model, content)
                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                final = make_stream_chunk(response_id, model, "", is_final=True)
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                yield "data: [DONE]\n\n"
                success = True
            finally:
                await _acct_dir.release(acct)
                kind = FeedbackKind.SUCCESS if success else FeedbackKind.SERVER_ERROR
                await _acct_dir.feedback(token, kind, int(spec.mode_id))

        return _sse_stream()

    # Non-streaming: collect all images first.
    try:
        finals = await _collect()
    finally:
        await _acct_dir.release(acct)
        await _acct_dir.feedback(token, FeedbackKind.SUCCESS, int(spec.mode_id))

    if chat_format:
        # Return as chat.completion with markdown image(s).
        content = "\n\n".join(
            _wrap_image_markdown(img, response_format) for img in finals
        )
        from .response import make_chat_response
        return make_chat_response(
            model, content,
            response_id = response_id,
            usage       = build_usage(0, 0),
        )

    # Standard images.generations response.
    data = []
    for img in finals:
        if response_format in ("b64_json", "base64"):
            data.append({"b64_json": img})
        else:
            data.append({"url": img})
    return {
        "created": int(time.time()),
        "data":    data,
    }


__all__ = ["generate", "resolve_aspect_ratio"]
