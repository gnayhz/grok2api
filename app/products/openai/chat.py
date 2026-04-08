"""Chat completion service — orchestrates account selection, reverse, streaming."""

import asyncio
import base64
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.platform.storage import image_files_dir
from app.control.account.runtime import get_refresh_service
from app.control.model.registry import resolve as resolve_model
from app.control.model.enums import ModeId
from app.control.account.enums import FeedbackKind
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy import get_proxy_runtime
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.reverse.protocol.xai_chat import build_chat_payload, classify_line, StreamAdapter
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from app.dataplane.reverse.transport.asset_upload import upload_from_input
from ._format import (
    make_response_id, make_stream_chunk, make_thinking_chunk, make_chat_response,
    estimate_tokens, build_usage,
)


async def _quota_sync(token: str, mode_id: int) -> None:
    """Fire-and-forget: fetch real quota after a successful call."""
    try:
        svc = get_refresh_service()
        if svc:
            await svc.refresh_call_async(token, mode_id)
    except Exception:
        pass


async def _fail_sync(token: str, mode_id: int) -> None:
    """Fire-and-forget: persist failure counter after a failed call."""
    try:
        svc = get_refresh_service()
        if svc:
            await svc.record_failure_async(token, mode_id)
    except Exception:
        pass


def _parse_retry_codes(s: str) -> frozenset[int]:
    """Parse a comma-separated list of HTTP status codes into a frozenset."""
    result: set[int] = set()
    for part in s.split(","):
        part = part.strip()
        if part.isdigit():
            result.add(int(part))
    return frozenset(result)


def _feedback_kind(exc: BaseException) -> "FeedbackKind":
    """Map an upstream exception to the appropriate FeedbackKind."""
    status = getattr(exc, "status", 0)
    if status == 429:
        return FeedbackKind.RATE_LIMITED
    if status == 401:
        return FeedbackKind.AUTH_FAILURE
    if status == 403:
        return FeedbackKind.FORBIDDEN
    return FeedbackKind.SERVER_ERROR


async def _download_image_bytes(token: str, url: str) -> tuple[bytes, str]:
    """Download image bytes via the shared asset transport used by /v1/images."""
    from app.dataplane.reverse.protocol.xai_assets import infer_content_type
    from app.dataplane.reverse.transport.assets import download_asset

    try:
        stream, content_type = await download_asset(token, url)
        chunks: list[bytes] = []
        async for chunk in stream:
            chunks.append(chunk)
    except UpstreamError:
        raise
    except Exception as exc:
        raise UpstreamError(f"Image download failed: {exc}") from exc
    return b"".join(chunks), (content_type or infer_content_type(url) or "image/jpeg")


def _save_image(raw: bytes, mime: str, image_id: str) -> str:
    """Save raw bytes to data/files/images/, return the file ID."""
    img_dir = image_files_dir()
    ext = ".png" if "png" in mime else ".jpg"
    path = img_dir / f"{image_id}{ext}"
    if not path.exists():
        path.write_bytes(raw)
    return image_id


async def _resolve_image(token: str, url: str, image_id: str) -> str:
    """Return the image embed text for the response body based on image_format config.

    Format values:
      grok_url  — raw CDN URL (no download)
      local_url — download + serve locally, return accessible URL
      grok_md   — ![image](grok_cdn_url) markdown
      local_md  — ![image](local_url) markdown
      base64    — ![image](data:...) markdown
    """
    cfg = get_config()
    fmt = cfg.get_str("features.image_format", "grok_url")

    # Backward compatibility: old "url" value → local_url
    if fmt == "url":
        fmt = "local_url"

    # Formats that don't need downloading
    if fmt == "grok_url":
        return url
    if fmt == "grok_md":
        return f"![image]({url})"

    # Formats that require downloading
    try:
        raw, mime = await _download_image_bytes(token, url)
    except Exception as exc:
        logger.warning("Image download failed, falling back to raw URL: {}", exc)
        return url

    if fmt == "base64":
        b64 = base64.b64encode(raw).decode()
        return f"![image](data:{mime};base64,{b64})"

    # local_url / local_md: save to disk and return local path
    file_id   = _save_image(raw, mime, image_id)
    app_url   = cfg.get_str("app.app_url", "").rstrip("/")
    local_url = f"{app_url}/v1/files/image?id={file_id}" if app_url else f"/v1/files/image?id={file_id}"

    if fmt == "local_url":
        return local_url
    return f"![image]({local_url})"  # local_md


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


async def _prepare_file_attachments(token: str, file_inputs: list[str]) -> list[str]:
    """Upload OpenAI-style multimodal inputs and return Grok chat attachment IDs."""
    attachments: list[str] = []
    for file_input in file_inputs:
        if not file_input:
            continue
        file_id, _file_uri = await upload_from_input(token, file_input)
        if file_id:
            attachments.append(file_id)
    return attachments


async def _stream_chat(
    token:      str,
    mode_id:    "ModeId",
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
    attachments = await _prepare_file_attachments(token, files)

    payload = build_chat_payload(
        message               = message,
        mode_id               = mode_id,
        file_attachments      = attachments,
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
                body = response.content.decode("utf-8", "replace")[:400]
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
    thinking:   bool | None = None,
    tools:      list[dict] | None = None,
    tool_choice: Any = None,
    temperature: float = 0.8,
    top_p:       float = 0.95,
    request_overrides: dict | None = None,
) -> dict | AsyncGenerator[str, None]:
    """Entry point for /v1/chat/completions.

    Returns an async generator for streaming, or a dict for non-streaming.
    Supports transparent retry with a different account on configured HTTP
    status codes (chat.retry_on_codes) up to chat.max_retries times.
    """
    cfg        = get_config()
    spec       = resolve_model(model)
    is_stream  = stream   if stream   is not None else cfg.get_bool("features.stream",   True)
    emit_think = thinking if thinking is not None else cfg.get_bool("features.thinking", True)

    logger.info("Chat request: model={} stream={} msgs={}", model, is_stream, len(messages))

    message, files = _extract_message(messages)
    if not message.strip():
        raise UpstreamError("Empty message after extraction", status=400)

    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")
    directory = _acct_dir

    max_retries   = cfg.get_int("retry.max_retries", 1)
    retry_codes   = _parse_retry_codes(cfg.get_str("retry.on_codes", "429,503"))
    response_id   = make_response_id()
    timeout_s     = cfg.get_float("chat.timeout", 120.0)

    tool_overrides: dict | None = None
    if tools:
        tool_overrides = {"tools": tools, "toolChoice": tool_choice or "auto"}

    # ── Streaming path ────────────────────────────────────────────────────────
    if is_stream:
        async def _run_stream() -> AsyncGenerator[str, None]:
            excluded: list[str] = []
            for attempt in range(max_retries + 1):
                acct = await directory.reserve(
                    pool_candidates = spec.pool_candidates(),
                    mode_id         = int(spec.mode_id),
                    now_s_override  = now_s(),
                    exclude_tokens  = excluded or None,
                )
                if acct is None:
                    raise RateLimitError("No available accounts for this model tier")

                token    = acct.token
                success  = False
                _retry   = False
                fail_exc: BaseException | None = None
                adapter  = StreamAdapter()

                try:
                    try:
                        ended = False
                        async for line in _stream_chat(
                            token             = token,
                            mode_id           = spec.mode_id,
                            message           = message,
                            files             = files,
                            tool_overrides    = tool_overrides,
                            request_overrides = request_overrides,
                            timeout_s         = timeout_s,
                        ):
                            event_type, data = classify_line(line)
                            logger.debug("SSE: type={} data_len={}", event_type, len(data))
                            if event_type == "done":
                                break
                            if event_type != "data" or not data:
                                continue
                            events = adapter.feed(data)
                            if not events:
                                logger.debug("StreamAdapter skip: data[:120]={}", data[:120])
                            for ev in events:
                                if ev.kind == "text":
                                    chunk = make_stream_chunk(response_id, model, ev.content)
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev.kind == "thinking" and emit_think:
                                    chunk = make_thinking_chunk(response_id, model, ev.content)
                                    yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                                elif ev.kind == "soft_stop":
                                    ended = True
                                    break
                            if ended:
                                break

                        for url, img_id in adapter.image_urls:
                            img_text = await _resolve_image(token, url, img_id)
                            chunk = make_stream_chunk(response_id, model, img_text + "\n")
                            yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                        references = adapter.references_suffix()
                        if references:
                            chunk = make_stream_chunk(response_id, model, references)
                            yield f"data: {orjson.dumps(chunk).decode()}\n\n"

                        final = make_stream_chunk(response_id, model, "", is_final=True)
                        yield f"data: {orjson.dumps(final).decode()}\n\n"
                        yield "data: [DONE]\n\n"
                        success = True
                        logger.info("Chat stream done (attempt {}/{}): model={} images={}",
                                    attempt + 1, max_retries + 1, model, len(adapter.image_urls))

                    except UpstreamError as exc:
                        fail_exc = exc
                        if exc.status in retry_codes and attempt < max_retries:
                            _retry = True
                            logger.warning("Chat stream retry {}/{}: status={} token={}...",
                                           attempt + 1, max_retries, exc.status, token[:8])
                        else:
                            raise

                finally:
                    await directory.release(acct)
                    kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
                    await directory.feedback(token, kind, int(spec.mode_id), now_s_val=now_s())
                    if success:
                        asyncio.create_task(_quota_sync(token, int(spec.mode_id)))
                    else:
                        asyncio.create_task(_fail_sync(token, int(spec.mode_id)))

                if success or not _retry:
                    return
                excluded.append(token)

        return _run_stream()

    # ── Non-streaming path ────────────────────────────────────────────────────
    excluded: list[str] = []
    adapter  = StreamAdapter()
    for attempt in range(max_retries + 1):
        acct = await directory.reserve(
            pool_candidates = spec.pool_candidates(),
            mode_id         = int(spec.mode_id),
            now_s_override  = now_s(),
            exclude_tokens  = excluded or None,
        )
        if acct is None:
            raise RateLimitError("No available accounts for this model tier")

        token    = acct.token
        success  = False
        _retry   = False
        fail_exc: BaseException | None = None
        adapter  = StreamAdapter()

        try:
            try:
                async for line in _stream_chat(
                    token             = token,
                    mode_id           = spec.mode_id,
                    message           = message,
                    files             = files,
                    tool_overrides    = tool_overrides,
                    request_overrides = request_overrides,
                    timeout_s         = timeout_s,
                ):
                    event_type, data = classify_line(line)
                    if event_type == "done":
                        break
                    if event_type != "data" or not data:
                        continue
                    ended = False
                    for ev in adapter.feed(data):
                        if ev.kind == "soft_stop":
                            ended = True
                            break
                    if ended:
                        break
                success = True

            except UpstreamError as exc:
                fail_exc = exc
                if exc.status in retry_codes and attempt < max_retries:
                    _retry = True
                    logger.warning("Chat retry {}/{}: status={} token={}...",
                                   attempt + 1, max_retries, exc.status, token[:8])
                else:
                    raise

        finally:
            await directory.release(acct)
            kind = FeedbackKind.SUCCESS if success else _feedback_kind(fail_exc) if fail_exc else FeedbackKind.SERVER_ERROR
            await directory.feedback(token, kind, int(spec.mode_id))
            if success:
                asyncio.create_task(_quota_sync(token, int(spec.mode_id)))
            else:
                asyncio.create_task(_fail_sync(token, int(spec.mode_id)))

        if success or not _retry:
            break
        excluded.append(token)

    full_text = "".join(adapter.text_buf)
    for url, img_id in adapter.image_urls:
        img_text = await _resolve_image(token, url, img_id)
        if full_text:
            full_text += "\n\n"
        full_text += img_text

    references = adapter.references_suffix()
    if references:
        full_text += references

    thinking_text = ("".join(adapter.thinking_buf) or None) if emit_think else None

    logger.info("Chat non-stream done (attempt {}/{}): model={} text_len={} think_len={} images={}",
                attempt + 1, max_retries + 1, model, len(full_text),
                len(thinking_text or ""), len(adapter.image_urls))

    pt = estimate_tokens(message) + 4
    ct = estimate_tokens(full_text)
    rt = estimate_tokens(thinking_text) if thinking_text else 0
    return make_chat_response(
        model, full_text,
        response_id       = response_id,
        reasoning_content = thinking_text,
        usage             = build_usage(pt, ct + rt, reasoning_tokens=rt),
    )


__all__ = ["completions"]
