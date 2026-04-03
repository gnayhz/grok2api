"""OpenAI-compatible response formatting utilities."""

from __future__ import annotations

import math
import os
import re
import time
from typing import Any

import orjson

_SEGMENT_RE = re.compile(r"\w+|[^\w\s]", re.UNICODE)
_PROMPT_OVERHEAD = 4


def make_response_id() -> str:
    return f"chatcmpl-{int(time.time() * 1000)}{os.urandom(4).hex()}"


def estimate_tokens(value: Any) -> int:
    """Lightweight token count estimate (UTF-8 bytes / 4, min 1 if non-empty)."""
    if value is None:
        return 0
    if not isinstance(value, str):
        try:
            value = orjson.dumps(value).decode()
        except Exception:
            value = str(value)
    text = value.strip()
    if not text:
        return 0
    byte_est    = math.ceil(len(text.encode()) / 4)
    segment_est = math.ceil(len(_SEGMENT_RE.findall(text)) * 0.75)
    return max(1, byte_est, segment_est)


def build_usage(prompt_tokens: int, completion_tokens: int) -> dict:
    pt = max(0, prompt_tokens)
    ct = max(0, completion_tokens)
    return {
        "prompt_tokens":     pt,
        "completion_tokens": ct,
        "total_tokens":      pt + ct,
        "prompt_tokens_details": {
            "cached_tokens": 0, "text_tokens": pt,
            "audio_tokens": 0,  "image_tokens": 0,
        },
        "completion_tokens_details": {
            "text_tokens": ct, "audio_tokens": 0, "reasoning_tokens": 0,
        },
    }


def make_stream_chunk(
    response_id: str,
    model:       str,
    content:     str,
    *,
    index:      int  = 0,
    role:       str  = "assistant",
    is_final:   bool = False,
    finish_reason: str | None = None,
    usage:      dict | None   = None,
) -> dict:
    choice: dict = {
        "index": index,
        "delta": {"role": role, "content": content},
    }
    if is_final:
        choice["finish_reason"] = finish_reason or "stop"

    chunk: dict = {
        "id":      response_id,
        "object":  "chat.completion.chunk",
        "created": int(time.time()),
        "model":   model,
        "choices": [choice],
    }
    if usage is not None:
        chunk["usage"] = usage
    return chunk


def make_chat_response(
    model:    str,
    content:  str,
    *,
    response_id: str | None = None,
    usage:       dict | None = None,
) -> dict:
    rid = response_id or make_response_id()
    pt  = estimate_tokens(content) + _PROMPT_OVERHEAD
    ct  = estimate_tokens(content)
    return {
        "id":      rid,
        "object":  "chat.completion",
        "created": int(time.time()),
        "model":   model,
        "choices": [{
            "index":         0,
            "message":       {"role": "assistant", "content": content},
            "finish_reason": "stop",
        }],
        "usage": usage or build_usage(pt, ct),
    }


__all__ = [
    "make_response_id", "estimate_tokens", "build_usage",
    "make_stream_chunk", "make_chat_response",
]
