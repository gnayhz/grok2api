"""XAI app-chat protocol — payload builder and response line classifier."""

from __future__ import annotations

import re
import uuid
from typing import Any

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.control.model.enums import ModeId

# ---------------------------------------------------------------------------
# Mode → modeId string map (derived from browser JS reverse)
# Applies to SuperGrok multi-agent models that require modeId field.
# ---------------------------------------------------------------------------
_GROK_MODE_IDS: dict[str, str] = {
    "MODEL_MODE_FAST":                  "fast",
    "MODEL_MODE_EXPERT":                "expert",
    "MODEL_MODE_HEAVY":                 "heavy",
    "MODEL_MODE_GROK_420":              "expert",
    "MODEL_MODE_GROK_4_1_THINKING":     "expert",
    "MODEL_MODE_GROK_4_1_MINI_THINKING": "expert",
}


def build_chat_payload(
    *,
    message:              str,
    model_name:           str,
    model_mode:           str | None         = None,
    file_attachments:     list[str]          = (),
    tool_overrides:       dict[str, Any]     | None = None,
    model_config_override: dict[str, Any]   | None = None,
    request_overrides:    dict[str, Any]    | None = None,
) -> dict[str, Any]:
    """Build the JSON payload for POST /rest/app-chat/conversations/new."""
    cfg = get_config()

    payload: dict[str, Any] = {
        "deviceEnvInfo": {
            "darkModeEnabled":  False,
            "devicePixelRatio": 2,
            "screenHeight":     1329,
            "screenWidth":      2056,
            "viewportHeight":   1083,
            "viewportWidth":    2056,
        },
        "disableMemory":               cfg.get_bool("app.disable_memory", False),
        "disableSearch":               False,
        "disableSelfHarmShortCircuit": False,
        "disableTextFollowUps":        False,
        "enableImageGeneration":       True,
        "enableImageStreaming":        True,
        "enableSideBySide":            True,
        "fileAttachments":             list(file_attachments),
        "forceConcise":                False,
        "forceSideBySide":             False,
        "imageAttachments":            [],
        "imageGenerationCount":        2,
        "isAsyncChat":                 False,
        "isReasoning":                 False,
        "message":                     message,
        "modelMode":                   model_mode,
        "modelName":                   model_name,
        "responseMetadata": {
            "requestModelDetails": {"modelId": model_name},
        },
        "returnImageBytes":            False,
        "returnRawGrokInXaiRequest":   False,
        "sendFinalMetadata":           True,
        "temporary":                   cfg.get_bool("app.temporary", True),
        "toolOverrides":               tool_overrides or {},
    }

    # For models that expose modeId, prefer it and strip modelName/modelMode
    # (mirrors browser front-end logic).
    if model_mode:
        mode_id = _GROK_MODE_IDS.get(model_mode)
        if mode_id:
            payload["modeId"] = mode_id
            payload.pop("modelName", None)
            payload.pop("modelMode", None)

    # Optional operator-supplied system instruction.
    custom = cfg.get_str("app.custom_instruction", "").strip()
    if custom:
        payload["customPersonality"] = custom

    if model_config_override:
        payload["responseMetadata"]["modelConfigOverride"] = model_config_override

    if request_overrides:
        payload.update({k: v for k, v in request_overrides.items() if v is not None})

    logger.debug(
        "Chat payload: model={} mode={} msg_len={} files={}",
        model_name, model_mode, len(message), len(file_attachments),
    )
    return payload


# ---------------------------------------------------------------------------
# Response line parsing
# ---------------------------------------------------------------------------

_RESULT_TAG_RE = re.compile(r'"result"\s*:\s*"([^"]*)"')


def classify_line(line: str) -> tuple[str, str]:
    """Return (event_type, data) for a raw SSE line.

    event_type: 'data' | 'error' | 'done' | 'skip'
    """
    line = line.strip()
    if not line:
        return "skip", ""
    if line.startswith("data:"):
        data = line[5:].strip()
        if data == "[DONE]":
            return "done", ""
        return "data", data
    if line.startswith("event:"):
        return "skip", ""
    return "skip", ""


def parse_token(data: str) -> str | None:
    """Extract text token from a single SSE data payload."""
    try:
        obj = orjson.loads(data)
    except Exception:
        return None

    result = obj.get("result")
    if not result:
        return None

    # Text streaming token.
    token = result.get("message", {}).get("token")
    if token is not None:
        return token

    # Complete message fallback.
    msg = result.get("message", {})
    if msg.get("finalMetadata"):
        return None
    return None


__all__ = [
    "build_chat_payload",
    "classify_line",
    "parse_token",
    "_GROK_MODE_IDS",
]
