"""XAI app-chat protocol — payload builder and SSE stream adapter."""

import re
from dataclasses import dataclass
from typing import Any

import orjson

from app.platform.logging.logger import logger
from app.platform.config.snapshot import get_config
from app.control.model.enums import ModeId, MODE_STRINGS
from app.dataplane.reverse.protocol.xai_chat_reasoning import ReasoningAggregator


def build_chat_payload(
    *,
    message:               str,
    mode_id:               ModeId,
    file_attachments:      list[str]        = (),
    tool_overrides:        dict[str, Any]   | None = None,
    model_config_override: dict[str, Any]   | None = None,
    request_overrides:     dict[str, Any]   | None = None,
) -> dict[str, Any]:
    """Build the JSON payload for POST /rest/app-chat/conversations/new."""
    cfg = get_config()

    payload: dict[str, Any] = {
        "collectionIds":               [],
        "connectors":                  [],
        "deviceEnvInfo": {
            "darkModeEnabled":  False,
            "devicePixelRatio": 2,
            "screenHeight":     1329,
            "screenWidth":      2056,
            "viewportHeight":   1083,
            "viewportWidth":    2056,
        },
        "disableMemory":               not cfg.get_bool("features.memory", False),
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
        "message":                     message,
        "modeId":                      MODE_STRINGS[mode_id],
        "responseMetadata":            {},
        "returnImageBytes":            False,
        "returnRawGrokInXaiRequest":   False,
        "searchAllConnectors":         False,
        "sendFinalMetadata":           True,
        "temporary":                   cfg.get_bool("features.temporary", True),
        "toolOverrides": tool_overrides or {
            "gmailSearch":           False,
            "googleCalendarSearch":  False,
            "outlookSearch":         False,
            "outlookCalendarSearch": False,
            "googleDriveSearch":     False,
        },
    }

    custom = cfg.get_str("features.custom_instruction", "").strip()
    if custom:
        payload["customPersonality"] = custom

    if model_config_override:
        payload["responseMetadata"]["modelConfigOverride"] = model_config_override

    if request_overrides:
        payload.update({k: v for k, v in request_overrides.items() if v is not None})

    logger.debug(
        "chat payload built: mode={} message_len={} file_count={}",
        MODE_STRINGS[mode_id], len(message), len(file_attachments),
    )
    return payload


# ---------------------------------------------------------------------------
# SSE line classification (unchanged)
# ---------------------------------------------------------------------------


def classify_line(line: str | bytes) -> tuple[str, str]:
    """Return (event_type, data) for a raw SSE line.

    event_type: 'data' | 'done' | 'skip'

    Handles both standard SSE ``data: {...}`` lines and raw JSON lines
    (upstream sometimes omits the ``data:`` prefix).
    """
    if isinstance(line, bytes):
        line = line.decode("utf-8", "replace")
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
    # Raw JSON line (no "data:" prefix) — treat as data.
    if line.startswith("{"):
        return "data", line
    return "skip", ""


# ---------------------------------------------------------------------------
# FrameEvent — single output event from StreamAdapter.feed()
# ---------------------------------------------------------------------------

@dataclass(slots=True)
class FrameEvent:
    """One parsed event produced by StreamAdapter."""

    kind: str
    """Event kind:
    - ``text``      — cleaned final text token  (content = token string)
    - ``thinking``  — Grok main-model thinking   (content = raw token)
    - ``image``     — generated image final URL   (content = full URL, image_id = upstream UUID)
    - ``image_progress`` — generated image progress (content = percent string, image_id = upstream UUID)
    - ``soft_stop`` — stream end signal
    - ``skip``      — filtered frame, do nothing
    """
    content: str = ""
    image_id: str = ""
    rollout_id: str = ""
    message_tag: str = ""
    message_step_id: int | None = None


# ---------------------------------------------------------------------------
# StreamAdapter — stateful SSE frame parser
# ---------------------------------------------------------------------------

_GROK_RENDER_RE = re.compile(
    r'<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"'
    r'[^>]*>.*?</grok:render>',
    re.DOTALL,
)

_IMAGE_BASE = "https://assets.grok.com/"


class StreamAdapter:
    """Parse upstream SSE frames and emit :class:`FrameEvent` objects.

    One instance per HTTP request.  Call :meth:`feed` for every ``data:``
    line; iterate over the returned list of events.
    """

    __slots__ = (
        "_card_cache",
        "_citation_order",
        "_citation_map",
        "_emitted_reasoning_keys",
        "_reasoning",
        "thinking_buf",
        "text_buf",
        "image_urls",
    )

    def __init__(self) -> None:
        self._card_cache: dict[str, dict] = {}
        self._citation_order: list[str] = []
        self._citation_map: dict[str, int] = {}
        self._emitted_reasoning_keys: set[str] = set()
        self._reasoning = ReasoningAggregator()
        self.thinking_buf: list[str] = []
        self.text_buf: list[str] = []
        self.image_urls: list[tuple[str, str]] = []   # [(url, imageUuid), ...]

    def references_suffix(self) -> str:
        """Return a stable, language-neutral reference list for collected citations."""
        if not self._citation_order:
            return ""
        lines = [f"[{index}] {url}" for index, url in enumerate(self._citation_order, start=1)]
        return "\n\n" + "\n".join(lines)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def feed(self, data: str) -> list[FrameEvent]:
        """Parse one JSON ``data:`` payload; return 0-N events."""
        try:
            obj = orjson.loads(data)
        except (orjson.JSONDecodeError, ValueError, TypeError):
            return []

        result = obj.get("result")
        if not result:
            return []
        resp = result.get("response")
        if not resp:
            return []

        events: list[FrameEvent] = []

        # ── cache every cardAttachment first ──────────────────────
        card_raw = resp.get("cardAttachment")
        if card_raw:
            events.extend(self._handle_card(card_raw))

        token   = resp.get("token")
        think   = resp.get("isThinking")
        tag     = resp.get("messageTag")
        rollout = resp.get("rolloutId")
        step_id = resp.get("messageStepId")

        if tag == "tool_usage_card":
            for line in self._summarize_tool_usage(resp, rollout=rollout, step_id=step_id):
                self._append_reasoning(
                    events,
                    line,
                    rollout=rollout,
                    tag=tag,
                    step_id=step_id,
                )
            return events   # card events (if any) already added

        # ── raw_function_result ───────────────────────────────────
        if tag == "raw_function_result":
            return events

        # ── toolUsageCardId-only follow-up frame ──────────────────
        if resp.get("toolUsageCardId") and not resp.get("webSearchResults") and not resp.get("codeExecutionResult"):
            return events

        # ── thinking token ────────────────────────────────────────
        if token is not None and think is True:
            for line in self._reasoning.on_thinking(
                str(token),
                tag=tag,
                rollout=rollout,
                step_id=step_id if isinstance(step_id, int) else None,
            ):
                self._append_reasoning(
                    events,
                    line,
                    rollout=rollout,
                    tag=tag,
                    step_id=step_id,
                )
            return events

        # ── final text token (needs cleaning) ─────────────────────
        if token is not None and think is not True and tag == "final":
            cleaned = self._clean_token(token)
            if cleaned:
                self.text_buf.append(cleaned)
                events.append(FrameEvent("text", cleaned))
            return events

        # ── end signals ───────────────────────────────────────────
        if resp.get("isSoftStop"):
            self._flush_pending_reasoning(events)
            events.append(FrameEvent("soft_stop"))
            return events

        if resp.get("finalMetadata"):
            self._flush_pending_reasoning(events)
            events.append(FrameEvent("soft_stop"))
            return events

        return events

    # ------------------------------------------------------------------
    # Card attachment handling
    # ------------------------------------------------------------------

    def _handle_card(self, card_raw: dict) -> list[FrameEvent]:
        """Cache card data; emit image event on progress=100."""
        try:
            jd = orjson.loads(card_raw["jsonData"])
        except (orjson.JSONDecodeError, ValueError, TypeError, KeyError):
            return []

        card_id = jd.get("id", "")
        self._card_cache[card_id] = jd

        chunk = jd.get("image_chunk")
        if chunk:
            progress = chunk.get("progress")
            uuid = chunk.get("imageUuid", "")
            events: list[FrameEvent] = []
            try:
                if progress is not None:
                    events.append(FrameEvent("image_progress", str(int(progress)), uuid))
            except (TypeError, ValueError):
                pass
            if chunk.get("progress") == 100 and not chunk.get("moderated"):
                url = _IMAGE_BASE + chunk["imageUrl"]
                self.image_urls.append((url, uuid))
                events.append(FrameEvent("image", url, uuid))
            return events

        return []

    # ------------------------------------------------------------------
    # Token cleaning — <grok:render> → markdown
    # ------------------------------------------------------------------

    def _clean_token(self, token: str) -> str:
        if "<grok:render" not in token:
            return token
        return _GROK_RENDER_RE.sub(self._render_replace, token)

    def _render_replace(self, m: re.Match) -> str:
        card_id     = m.group(1)
        render_type = m.group(3)
        card = self._card_cache.get(card_id)
        if not card:
            return ""

        if render_type == "render_searched_image":
            img   = card.get("image", {})
            title = img.get("title", "image")
            thumb = img.get("thumbnail") or img.get("original", "")
            link  = img.get("link", "")
            if link:
                return f"[![{title}]({thumb})]({link})"
            return f"![{title}]({thumb})"

        if render_type == "render_generated_image":
            return ""   # actual URL emitted by progress=100 card frame

        if render_type == "render_inline_citation":
            url = card.get("url", "")
            if not url:
                return ""
            index = self._citation_map.get(url)
            if index is None:
                self._citation_order.append(url)
                index = len(self._citation_order)
                self._citation_map[url] = index
            return f" [{index}]"

        return ""

    def _append_reasoning(
        self,
        events: list[FrameEvent],
        line: str,
        *,
        rollout: str | None,
        tag: str | None,
        step_id: Any,
    ) -> None:
        text = line.strip()
        if not text:
            return

        key = self._normalize_key(text)
        if key in self._emitted_reasoning_keys:
            return

        self._emitted_reasoning_keys.add(key)
        formatted = text if text.endswith("\n") else text + "\n"
        self.thinking_buf.append(formatted)
        events.append(FrameEvent(
            "thinking",
            formatted,
            rollout_id=rollout or "",
            message_tag=tag or "",
            message_step_id=step_id if isinstance(step_id, int) else None,
        ))

    def _flush_pending_reasoning(self, events: list[FrameEvent]) -> None:
        for line in self._reasoning.finalize():
            self._append_reasoning(
                events,
                line,
                rollout="",
                tag="summary",
                step_id=None,
            )

    def _summarize_tool_usage(self, resp: dict[str, Any], *, rollout: str | None, step_id: int | None) -> list[str]:
        card = resp.get("toolUsageCard")
        if not isinstance(card, dict):
            return []

        tool_name = ""
        args: dict[str, Any] = {}
        for key, value in card.items():
            if key == "toolUsageCardId" or not isinstance(value, dict):
                continue
            tool_name = re.sub(r"(?<!^)([A-Z])", r"_\1", key).lower()
            raw_args = value.get("args")
            if isinstance(raw_args, dict):
                args = raw_args
            break

        if not tool_name:
            return []

        return self._reasoning.on_tool_usage(
            tool_name,
            args,
            rollout=rollout,
            step_id=step_id,
        )

    def _normalize_key(self, text: str) -> str:
        lowered = text.lower()
        lowered = re.sub(r"https?://\S+", "", lowered)
        lowered = re.sub(r"[^\w\u4e00-\u9fff]+", "", lowered)
        return lowered


__all__ = [
    "build_chat_payload",
    "classify_line",
    "FrameEvent",
    "StreamAdapter",
]
