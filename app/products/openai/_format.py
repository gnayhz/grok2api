"""Response formatting utilities — pure functions, no async, no IO.

Two sections:
  - Chat Completions format  (make_response_id, make_stream_chunk, …)
  - Responses API format     (make_resp_id, make_resp_object, …)
"""

import math
import os
import re
import time
from typing import Any

import orjson

_SEGMENT_RE     = re.compile(r"\w+|[^\w\s]", re.UNICODE)
_PROMPT_OVERHEAD = 4


# ---------------------------------------------------------------------------
# Shared
# ---------------------------------------------------------------------------

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


# ---------------------------------------------------------------------------
# Chat Completions format
# ---------------------------------------------------------------------------

def make_response_id() -> str:
    return f"chatcmpl-{int(time.time() * 1000)}{os.urandom(4).hex()}"


def build_usage(prompt_tokens: int, completion_tokens: int, *, reasoning_tokens: int = 0) -> dict:
    pt = max(0, prompt_tokens)
    ct = max(0, completion_tokens)
    rt = max(0, reasoning_tokens)
    return {
        "prompt_tokens":     pt,
        "completion_tokens": ct,
        "total_tokens":      pt + ct,
        "prompt_tokens_details": {
            "cached_tokens": 0, "text_tokens": pt,
            "audio_tokens":  0, "image_tokens": 0,
        },
        "completion_tokens_details": {
            "text_tokens": ct - rt, "audio_tokens": 0, "reasoning_tokens": rt,
        },
    }


def make_stream_chunk(
    response_id: str,
    model:       str,
    content:     str,
    *,
    index:         int       = 0,
    role:          str       = "assistant",
    is_final:      bool      = False,
    finish_reason: str | None = None,
    usage:         dict | None = None,
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


def make_thinking_chunk(
    response_id: str,
    model:       str,
    content:     str,
    *,
    index: int = 0,
    role:  str = "assistant",
) -> dict:
    """Stream chunk carrying reasoning_content (DeepSeek-R1 style thinking delta)."""
    return {
        "id":      response_id,
        "object":  "chat.completion.chunk",
        "created": int(time.time()),
        "model":   model,
        "choices": [{
            "index": index,
            "delta": {"role": role, "reasoning_content": content},
        }],
    }


def make_chat_response(
    model:   str,
    content: str,
    *,
    response_id:       str | None  = None,
    usage:             dict | None = None,
    reasoning_content: str | None  = None,
) -> dict:
    rid = response_id or make_response_id()
    pt  = estimate_tokens(content) + _PROMPT_OVERHEAD
    ct  = estimate_tokens(content)
    rt  = estimate_tokens(reasoning_content) if reasoning_content else 0
    ct += rt

    msg: dict = {"role": "assistant", "content": content}
    if reasoning_content:
        msg["reasoning_content"] = reasoning_content

    return {
        "id":      rid,
        "object":  "chat.completion",
        "created": int(time.time()),
        "model":   model,
        "choices": [{
            "index":         0,
            "message":       msg,
            "finish_reason": "stop",
        }],
        "usage": usage or build_usage(pt, ct, reasoning_tokens=rt),
    }


# ---------------------------------------------------------------------------
# Responses API format
# ---------------------------------------------------------------------------

def make_resp_id(prefix: str) -> str:
    """Generate a Responses API item ID, e.g. resp_xxx / rs_xxx / msg_xxx."""
    return f"{prefix}_{int(time.time() * 1000)}{os.urandom(4).hex()}"


def build_resp_usage(input_tokens: int, output_tokens: int, reasoning_tokens: int = 0) -> dict:
    return {
        "input_tokens":  max(0, input_tokens),
        "output_tokens": max(0, output_tokens),
        "total_tokens":  max(0, input_tokens + output_tokens),
        "output_tokens_details": {"reasoning_tokens": max(0, reasoning_tokens)},
    }


def make_resp_object(
    response_id: str,
    model:       str,
    status:      str,
    output:      list[dict],
    usage:       dict | None = None,
) -> dict:
    obj: dict = {
        "id":         response_id,
        "object":     "response",
        "created_at": int(time.time()),
        "status":     status,
        "model":      model,
        "output":     output,
    }
    if usage is not None:
        obj["usage"] = usage
    return obj


def format_sse(event: str, data: dict) -> str:
    """Encode a single Responses API SSE event frame."""
    return f"event: {event}\ndata: {orjson.dumps(data).decode()}\n\n"


__all__ = [
    # shared
    "estimate_tokens",
    # chat completions
    "make_response_id", "build_usage",
    "make_stream_chunk", "make_thinking_chunk", "make_chat_response",
    # responses api
    "make_resp_id", "build_resp_usage", "make_resp_object", "format_sse",
]
