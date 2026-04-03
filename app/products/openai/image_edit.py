"""Image edit service — app-chat with file attachments + image generation.

Uploads input images as file attachments, sends a chat request with the
imageGenerationCount override, and parses generated image URLs from the SSE
response stream.
"""

from __future__ import annotations

import time
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError, ValidationError
from app.platform.runtime.clock import now_s
from app.control.model.registry import resolve as resolve_model
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.protocol.xai_chat import build_chat_payload, classify_line
from app.dataplane.reverse.transport.asset_upload import upload_from_input
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from .response import make_response_id, make_stream_chunk, make_chat_response, build_usage

# ---------------------------------------------------------------------------
# Upstream model / mode for image-edit requests
# ---------------------------------------------------------------------------
_EDIT_GROK_MODEL = "grok-4"
_EDIT_GROK_MODE  = "MODEL_MODE_AUTO"


# ---------------------------------------------------------------------------
# Image URL extraction from SSE response
# ---------------------------------------------------------------------------

def _collect_image_urls(obj: Any) -> list[str]:
    """Recursively collect generated image URLs from a parsed SSE object."""
    urls: list[str] = []
    seen: set[str]  = set()

    def _add(url: str) -> None:
        if url and url not in seen:
            seen.add(url)
            urls.append(url)

    def _walk(value: Any) -> None:
        if isinstance(value, dict):
            for key, item in value.items():
                if key in {"generatedImageUrls", "imageUrls", "imageURLs"}:
                    if isinstance(item, list):
                        for url in item:
                            if isinstance(url, str):
                                _add(url)
                    elif isinstance(item, str):
                        _add(item)
                else:
                    _walk(item)
        elif isinstance(value, list):
            for item in value:
                _walk(item)

    _walk(obj)
    return urls


def _parse_edit_line(data: str) -> list[str]:
    """Extract image URLs from a single SSE data payload (image-edit format)."""
    try:
        obj = orjson.loads(data)
    except Exception:
        return []

    result = obj.get("result") or {}

    # New-style: result.response.modelResponse
    response = result.get("response") or {}
    model_response = response.get("modelResponse")
    if model_response:
        urls = _collect_image_urls(model_response)
        if urls:
            return urls

    # Old-style / fallback: result.message with generatedImages
    message = result.get("message") or {}
    return _collect_image_urls(message)


# ---------------------------------------------------------------------------
# SSE stream runner (raw lines)
# ---------------------------------------------------------------------------

async def _run_stream(
    token:            str,
    message:          str,
    file_ids:         list[str],
    image_count:      int,
    *,
    timeout_s: float = 120.0,
) -> AsyncGenerator[str, None]:
    """Yield raw SSE lines from the Grok app-chat endpoint."""
    proxy   = await get_proxy_runtime()
    lease   = await proxy.acquire()

    payload = build_chat_payload(
        message          = message,
        model_name       = _EDIT_GROK_MODEL,
        model_mode       = _EDIT_GROK_MODE,
        file_attachments = file_ids,
        request_overrides = {"imageGenerationCount": max(1, image_count)},
    )
    headers = build_http_headers(token, lease=lease)
    kwargs  = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            CHAT,
            headers = headers,
            data    = orjson.dumps(payload),
            timeout = timeout_s,
            stream  = True,
        )
        if response.status_code != 200:
            body = (await response.aread()).decode("utf-8", "replace")[:300]
            raise UpstreamError(
                f"Image-edit upstream returned {response.status_code}",
                status = response.status_code,
                body   = body,
            )
        async for line in response.aiter_lines():
            yield line


# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------

async def edit(
    *,
    model:           str,
    messages:        list[dict],
    n:               int  = 1,
    size:            str  = "1024x1024",
    response_format: str  = "url",
    stream:          bool = False,
    chat_format:     bool = False,
) -> dict | AsyncGenerator[str, None]:
    """Entry point for image-edit requests (model capability IMAGE_EDIT).

    Extracts prompt text and image inputs from *messages*, uploads images as
    file attachments, then sends a chat request that produces generated images.

    Returns:
      Non-streaming: OpenAI images.generations dict (or chat dict if chat_format=True).
      Streaming:     async generator of SSE strings.
    """
    cfg       = get_config()
    spec      = resolve_model(model)
    timeout_s = cfg.get_float("chat.timeout", 120.0)

    # Extract prompt and image inputs from messages.
    prompt = ""
    image_inputs: list[str] = []

    for msg in reversed(messages):
        content = msg.get("content", "")
        if isinstance(content, str) and content.strip():
            if not prompt:
                prompt = content.strip()
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text" and not prompt:
                    prompt = (block.get("text") or "").strip()
                elif btype == "image_url":
                    url = (block.get("image_url") or {}).get("url", "")
                    if url:
                        image_inputs.append(url)

    if not prompt:
        raise ValidationError("Image edit requires a non-empty text prompt", param="messages")
    if not image_inputs:
        raise ValidationError(
            "Image edit requires at least one image_url content block", param="messages"
        )

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
        raise RateLimitError("No available accounts for image edit")

    token       = acct.token
    response_id = make_response_id()

    try:
        # Upload images as file attachments (cap at 3 images like the old service).
        imgs_to_upload = image_inputs[-3:] if len(image_inputs) > 3 else image_inputs
        file_ids: list[str] = []
        for img_input in imgs_to_upload:
            try:
                fid, _ = await upload_from_input(token, img_input)
                if fid:
                    file_ids.append(fid)
            except Exception as exc:
                logger.warning("Image edit upload failed for input: {}", exc)

        if not file_ids:
            raise UpstreamError("All image uploads failed; cannot proceed with image edit")

    except Exception:
        await _acct_dir.release(acct)
        raise

    # -----------------------------------------------------------------------
    # Streaming path
    # -----------------------------------------------------------------------
    if stream:
        async def _sse_stream() -> AsyncGenerator[str, None]:
            success = False
            try:
                final_urls: list[str] = []
                async for line in _run_stream(
                    token, prompt, file_ids, n, timeout_s=timeout_s,
                ):
                    ev_type, data = classify_line(line)
                    if ev_type == "done":
                        break
                    if ev_type != "data" or not data:
                        continue
                    urls = _parse_edit_line(data)
                    for url in urls:
                        if url not in final_urls:
                            final_urls.append(url)

                for url in final_urls[:n]:
                    content = _format_image(url, response_format, chat_format)
                    chunk   = make_stream_chunk(response_id, model, content)
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

    # -----------------------------------------------------------------------
    # Non-streaming path
    # -----------------------------------------------------------------------
    try:
        final_urls: list[str] = []
        async for line in _run_stream(token, prompt, file_ids, n, timeout_s=timeout_s):
            ev_type, data = classify_line(line)
            if ev_type == "done":
                break
            if ev_type != "data" or not data:
                continue
            for url in _parse_edit_line(data):
                if url not in final_urls:
                    final_urls.append(url)
    finally:
        await _acct_dir.release(acct)
        await _acct_dir.feedback(token, FeedbackKind.SUCCESS, int(spec.mode_id))

    images = final_urls[:n]

    if chat_format:
        content = "\n\n".join(_format_image(u, response_format, chat_format=True) for u in images)
        return make_chat_response(
            model, content,
            response_id = response_id,
            usage       = build_usage(0, 0),
        )

    data_list = [
        {"url": u} if response_format == "url" else {"b64_json": u}
        for u in images
    ]
    return {"created": int(time.time()), "data": data_list}


def _format_image(url: str, response_format: str, chat_format: bool) -> str:
    """Format a single image URL/b64 for inclusion in a response."""
    if chat_format:
        if response_format == "url":
            return f"![image]({url})"
        return f"![image](data:image/png;base64,{url})"
    return url


__all__ = ["edit"]
