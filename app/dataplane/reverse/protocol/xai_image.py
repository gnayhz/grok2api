"""XAI Imagine WebSocket protocol — message builders and image classifier."""

from __future__ import annotations

import re
import time
import uuid
from typing import Any, Optional


_URL_PATTERN = re.compile(r"/images/([a-f0-9-]+)\.(png|jpg|jpeg)", re.IGNORECASE)

WS_IMAGINE_URL = "wss://grok.com/ws/imagine/listen"


def build_request_message(
    request_id:   str,
    prompt:       str,
    aspect_ratio: str  = "2:3",
    enable_nsfw:  bool = True,
) -> dict[str, Any]:
    """Build the WebSocket conversation.item.create message."""
    return {
        "type":      "conversation.item.create",
        "timestamp": int(time.time() * 1000),
        "item": {
            "type": "message",
            "content": [{
                "requestId": request_id,
                "text":      prompt,
                "type":      "input_text",
                "properties": {
                    "section_count":   0,
                    "is_kids_mode":    False,
                    "enable_nsfw":     enable_nsfw,
                    "skip_upsampler":  False,
                    "is_initial":      False,
                    "aspect_ratio":    aspect_ratio,
                },
            }],
        },
    }


def classify_image(
    url:              str,
    blob:             str,
    *,
    final_min_bytes:  int = 50_000,
    medium_min_bytes: int = 5_000,
) -> dict[str, Any] | None:
    """Classify a WebSocket image message into stage metadata.

    Returns a dict with keys: type, image_id, ext, stage, blob, blob_size, url, is_final.
    Returns None if url or blob are empty.
    """
    if not url or not blob:
        return None

    match     = _URL_PATTERN.search(url)
    image_id  = match.group(1) if match else uuid.uuid4().hex
    ext       = match.group(2).lower() if match else "png"
    blob_size = len(blob)
    is_final  = blob_size >= final_min_bytes

    if is_final:
        stage = "final"
    elif blob_size > medium_min_bytes:
        stage = "medium"
    else:
        stage = "preview"

    return {
        "type":      "image",
        "image_id":  image_id,
        "ext":       ext,
        "stage":     stage,
        "blob":      blob,
        "blob_size": blob_size,
        "url":       url,
        "is_final":  is_final,
    }


__all__ = ["WS_IMAGINE_URL", "build_request_message", "classify_image"]
