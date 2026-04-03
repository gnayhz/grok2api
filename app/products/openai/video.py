"""Video generation service — app-chat SSE reverse with video config override."""

from __future__ import annotations

import math
import re
import time
from typing import Any, AsyncGenerator

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.platform.errors import RateLimitError, UpstreamError
from app.platform.runtime.clock import now_s
from app.control.model.registry import resolve as resolve_model
from app.control.model.enums import ModeId, Tier
from app.control.account.enums import FeedbackKind
from app.dataplane.reverse.protocol.xai_chat import build_chat_payload, classify_line
from app.dataplane.reverse.runtime.endpoint_table import CHAT
from app.dataplane.proxy.adapters.headers import build_http_headers
from app.dataplane.proxy.adapters.session import ResettableSession, build_session_kwargs
from app.dataplane.proxy import get_proxy_runtime
from .response import make_response_id, make_stream_chunk, make_chat_response, build_usage

_POST_ID_RE = re.compile(r"/generated/([0-9a-fA-F-]{32,36})/")

_PRESET_FLAGS = {
    "fun":    "--mode=extremely-crazy",
    "normal": "--mode=normal",
    "spicy":  "--mode=extremely-spicy-or-crazy",
    "custom": "--mode=custom",
}


def _build_message(prompt: str, preset: str) -> str:
    flag = _PRESET_FLAGS.get(preset, "--mode=custom")
    return f"{prompt} {flag}".strip()


def _build_video_model_config(
    parent_post_id: str,
    *,
    aspect_ratio:    str,
    resolution_name: str,
    video_length:    int,
) -> dict[str, Any]:
    return {
        "modelMap": {
            "videoGenModelConfig": {
                "aspectRatio":    aspect_ratio,
                "parentPostId":   parent_post_id,
                "resolutionName": resolution_name,
                "videoLength":    video_length,
            }
        }
    }


def _round_length(target: int, *, is_super: bool) -> int:
    return 10 if is_super and target >= 10 else 6


def _build_rounds(target: int, *, is_super: bool) -> list[int]:
    """Return list of per-round video_length values."""
    base = _round_length(target, is_super=is_super)
    extras = int(math.ceil(max(target - base, 0) / base))
    return [base] + [base] * extras


async def _run_chat_stream(
    token:                str,
    message:              str,
    grok_model:           str,
    grok_mode:            str,
    model_config_override: dict | None,
    *,
    timeout_s: float,
) -> AsyncGenerator[str, None]:
    proxy = await get_proxy_runtime()
    lease = await proxy.acquire()

    payload = build_chat_payload(
        message                = message,
        model_name             = grok_model,
        model_mode             = grok_mode,
        model_config_override  = model_config_override,
    )
    headers = build_http_headers(token, lease=lease)
    kwargs  = build_session_kwargs(lease=lease)

    async with ResettableSession(**kwargs) as session:
        response = await session.post(
            CHAT, headers=headers, data=orjson.dumps(payload),
            timeout=timeout_s, stream=True,
        )
        if response.status_code != 200:
            body = (await response.aread()).decode("utf-8", "replace")[:300]
            raise UpstreamError(f"Video upstream {response.status_code}", status=response.status_code, body=body)
        async for line in response.aiter_lines():
            yield line


def _extract_video_url(data: str) -> str | None:
    try:
        obj = orjson.loads(data)
    except Exception:
        return None
    result = obj.get("result") or {}
    # Video URL appears in streamingState or message metadata.
    video_url = (
        result.get("message", {}).get("videoUrl")
        or result.get("streamingState", {}).get("videoUrl")
        or result.get("videoUrl")
    )
    return video_url if isinstance(video_url, str) and video_url else None


def _extract_post_id(video_url: str) -> str:
    m = _POST_ID_RE.search(video_url)
    return m.group(1) if m else ""


async def completions(
    *,
    model:        str,
    messages:     list[dict],
    stream:       bool | None = None,
    aspect_ratio: str         = "3:2",
    video_length: int         = 6,
    resolution:   str         = "480p",
    preset:       str         = "custom",
) -> dict | AsyncGenerator[str, None]:
    """Entry point for video generation requests."""
    cfg       = get_config()
    spec      = resolve_model(model)
    is_stream = stream if stream is not None else cfg.get_bool("app.stream", False)
    timeout_s = cfg.get_float("video.timeout", 180.0)
    is_super  = spec.tier == Tier.SUPER

    # Determine grok mode string.
    _MODE_MAP = {ModeId.AUTO: "MODEL_MODE_AUTO", ModeId.FAST: "MODEL_MODE_FAST", ModeId.EXPERT: "MODEL_MODE_EXPERT"}
    grok_mode = _MODE_MAP.get(spec.mode_id, "MODEL_MODE_AUTO")

    # Extract prompt from messages.
    prompt = ""
    for msg in reversed(messages):
        content = msg.get("content", "")
        if isinstance(content, str) and content.strip():
            prompt = content.strip()
            break
        if isinstance(content, list):
            parts = [b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"]
            if parts:
                prompt = " ".join(p.strip() for p in parts if p.strip())
                break

    if not prompt:
        raise UpstreamError("Video prompt cannot be empty", status=400)

    message = _build_message(prompt, preset)

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
        raise RateLimitError("No available accounts for video generation")

    token       = acct.token
    response_id = make_response_id()
    rounds      = _build_rounds(video_length, is_super=is_super)

    async def _run_video() -> str:
        """Execute all rounds and return the final video URL."""
        parent_post_id = ""
        video_url      = ""

        for round_idx, round_len in enumerate(rounds):
            model_cfg = _build_video_model_config(
                parent_post_id,
                aspect_ratio    = aspect_ratio,
                resolution_name = resolution,
                video_length    = round_len,
            )
            logger.info(
                "Video round {}/{}: len={}s resolution={} parent={}",
                round_idx + 1, len(rounds), round_len, resolution, parent_post_id or "<seed>",
            )

            async for line in _run_chat_stream(
                token, message, spec.model_name, grok_mode, model_cfg, timeout_s=timeout_s,
            ):
                ev_type, data = classify_line(line)
                if ev_type != "data" or not data:
                    continue
                url = _extract_video_url(data)
                if url:
                    video_url      = url
                    parent_post_id = _extract_post_id(url)

        return video_url

    if is_stream:
        async def _sse() -> AsyncGenerator[str, None]:
            success = False
            try:
                video_url = await _run_video()
                content   = f"![video]({video_url})" if video_url else ""
                chunk     = make_stream_chunk(response_id, model, content)
                yield f"data: {orjson.dumps(chunk).decode()}\n\n"
                final = make_stream_chunk(response_id, model, "", is_final=True)
                yield f"data: {orjson.dumps(final).decode()}\n\n"
                yield "data: [DONE]\n\n"
                success = True
            finally:
                await _acct_dir.release(acct)
                kind = FeedbackKind.SUCCESS if success else FeedbackKind.SERVER_ERROR
                await _acct_dir.feedback(token, kind, int(spec.mode_id))

        return _sse()

    try:
        video_url = await _run_video()
    finally:
        await _acct_dir.release(acct)
        await _acct_dir.feedback(token, FeedbackKind.SUCCESS, int(spec.mode_id))

    content = f"![video]({video_url})" if video_url else ""
    return make_chat_response(model, content, response_id=response_id, usage=build_usage(0, 0))


__all__ = ["completions"]
