"""Chat completion service — orchestrates account selection, reverse, streaming."""

from __future__ import annotations

import asyncio
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.control.model.registry import resolve as resolve_model
from app.control.account.enums import FeedbackKind
from app.dataplane.account import AccountDirectory
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from app.dataplane.reverse.protocol.xai_chat import build_chat_payload, classify_line
from app.dataplane.proxy.adapters.headers import build_http_headers
from .response import (
    make_response_id, make_stream_chunk, make_chat_response,
    estimate_tokens, build_usage,
)


async def _quota_sync(token: str, mode_id: int) -> None:
    """Fire-and-forget: fetch real quota after a successful call."""
    try:
        from app.main import app as _app
        svc = getattr(_app.state, "refresh_service", None)
        if svc:
            await svc.refresh_call_async(token, mode_id)
    except Exception:
        pass


def _extract_message(messages: list[dict]) -> tuple[str, list[str]]:
    """Flatten OpenAI messages into a single prompt string + file attachments."""
    parts: list[str] = []
    files: list[str] = []

    for msg in messages:
        role    = msg.get("role", "user")
        content = msg.get("content", "")

        if isinstance(content, str):
            if content.strip():
                parts.append(f"[{role}]: {content.strip()}")
        elif isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "text":
                    text = (block.get("text") or "").strip()
                    if text:
                        parts.append(f"[{role}]: {text}")
                elif btype == "image_url":
                    url = (block.get("image_url") or {}).get("url", "")
                    if url:
                        files.append(url)
                elif btype in ("input_audio", "file"):
                    inner = block.get(btype) or {}
                    data  = inner.get("data") or inner.get("file_data", "")
                    if data:
                        files.append(data)

        tool_calls = msg.get("tool_calls")
        if role == "assistant" and tool_calls:
            parts.append(f"[assistant tool_calls]: {orjson.dumps(tool_calls).decode()}")

    return "\n\n".join(parts), files


async def _stream_chat(
    token:      str,
    model_name: str,
    grok_model: str,
    grok_mode:  str,
    message:    str,
    files:      list[str],
    *,
    tool_overrides:       dict | None = None,
    model_config_override: dict | None = None,
    request_overrides:    dict | None = None,
    timeout_s:            float       = 120.0,
) -> AsyncGenerator[str, None]:
    """Yield raw SSE lines from the Grok app-chat endpoint."""
    proxy   = await get_proxy_runtime()
    lease   = await proxy.acquire()

    payload = build_chat_payload(
        message               = message,
        model_name            = grok_model,
        model_mode            = grok_mode,
        file_attachments      = files,
        tool_overrides        = tool_overrides,
        model_config_override = model_config_override,
        request_overrides     = request_overrides,
    )
    payload_bytes = orjson.dumps(payload)

    headers = build_http_headers(
        token,
        content_type = "application/json",
        origin       = "https://grok.com",
        referer      = "https://grok.com/",
        lease        = lease,
    )
    session_kwargs = build_session_kwargs(lease=lease)

    async with ResettableSession(**session_kwargs) as session:
        response = await session.post(
            CHAT,
            headers = headers,
            data    = payload_bytes,
            timeout = timeout_s,
            stream  = True,
        )

        if response.status_code != 200:
            try:
                body = (await response.aread()).decode("utf-8", "replace")[:400]
            except Exception:
                body = ""
            raise UpstreamError(
                f"Chat upstream returned {response.status_code}",
                status = response.status_code,
                body   = body,
            )

        async for line in response.aiter_lines():
            yield line


async def completions(
    *,
    model:      str,
    messages:   list[dict],
    stream:     bool | None = None,
    tools:      list[dict] | None = None,
    tool_choice: Any = None,
    temperature: float = 0.8,
    top_p:       float = 0.95,
    request_overrides: dict | None = None,
) -> dict | AsyncGenerator[str, None]:
    """Entry point for /v1/chat/completions.

    Returns an async generator for streaming, or a dict for non-streaming.
    """
    cfg       = get_config()
    spec      = resolve_model(model)
    is_stream = stream if stream is not None else cfg.get_bool("app.stream", True)

    # Map mode_id to upstream modeId string.
    from app.control.model.enums import ModeId
    _MODE_TO_UPSTREAM: dict[ModeId, str] = {
        ModeId.AUTO:   "MODEL_MODE_AUTO",
        ModeId.FAST:   "MODEL_MODE_FAST",
        ModeId.EXPERT: "MODEL_MODE_EXPERT",
    }
    grok_mode = _MODE_TO_UPSTREAM.get(spec.mode_id, "MODEL_MODE_AUTO")

    message, files = _extract_message(messages)
    if not message.strip():
        raise UpstreamError("Empty message after extraction", status=400)

    # Select account via the module-level singleton (bootstrapped at startup).
    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    ts      = now_s()
    acct    = await directory.reserve(
        pool_id = spec.pool_id(),
        mode_id = int(spec.mode_id),
        now_s_override = ts,
    )
    if acct is None:
        raise RateLimitError("No available accounts for this model tier")

    token       = acct.token
    response_id = make_response_id()

    # Tool override payload.
    tool_overrides: dict | None = None
    if tools:
        tool_overrides = {"tools": tools, "toolChoice": tool_choice or "auto"}

    timeout_s = cfg.get_float("chat.timeout", 120.0)

    async def _run_stream() -> AsyncGenerator[str, None]:
        success = False
        try:
            async for line in _stream_chat(
                token      = token,
                model_name = model,
                grok_model = spec.model_name,
                grok_mode  = grok_mode,
                message    = message,
                files      = files,
                tool_overrides       = tool_overrides,
                request_overrides    = request_overrides,
                timeout_s            = timeout_s,
            ):
                event_type, data = classify_line(line)
                if event_type == "done":
                    break
                if event_type != "data" or not data:
                    continue

                # Parse token from SSE data.
                try:
                    obj    = orjson.loads(data)
                    result = obj.get("result") or {}
                    token_text = result.get("message", {}).get("token")
                    if token_text is None:
                        continue
                except Exception:
                    continue

                chunk = make_stream_chunk(response_id, model, token_text)
                yield f"data: {orjson.dumps(chunk).decode()}\n\n"

            # Final chunk.
            final = make_stream_chunk(response_id, model, "", is_final=True)
            yield f"data: {orjson.dumps(final).decode()}\n\n"
            yield "data: [DONE]\n\n"
            success = True

        finally:
            await directory.release(acct)
            kind = FeedbackKind.SUCCESS if success else FeedbackKind.SERVER_ERROR
            await directory.feedback(
                token, kind, int(spec.mode_id), now_s_val=now_s()
            )
            if success:
                asyncio.create_task(_quota_sync(token, int(spec.mode_id)))

    if is_stream:
        return _run_stream()

    # Non-streaming: collect full response.
    full_text = ""
    async for line in _stream_chat(
        token      = token,
        model_name = model,
        grok_model = spec.model_name,
        grok_mode  = grok_mode,
        message    = message,
        files      = files,
        tool_overrides    = tool_overrides,
        request_overrides = request_overrides,
        timeout_s         = timeout_s,
    ):
        event_type, data = classify_line(line)
        if event_type == "done":
            break
        if event_type != "data" or not data:
            continue
        try:
            obj    = orjson.loads(data)
            result = obj.get("result") or {}
            tok    = result.get("message", {}).get("token")
            if tok:
                full_text += tok
        except Exception:
            continue

    await directory.release(acct)
    await directory.feedback(token, FeedbackKind.SUCCESS, int(spec.mode_id))
    asyncio.create_task(_quota_sync(token, int(spec.mode_id)))

    pt = estimate_tokens(message) + 4
    ct = estimate_tokens(full_text)
    return make_chat_response(
        model, full_text,
        response_id = response_id,
        usage       = build_usage(pt, ct),
    )


__all__ = ["completions"]
