"""OpenAI-compatible API router (/v1/*)."""

from __future__ import annotations

from typing import AsyncGenerator, AsyncIterable

import orjson
from fastapi import APIRouter, Depends
from fastapi.responses import JSONResponse, StreamingResponse

from app.platform.auth.middleware import verify_api_key
from app.platform.errors import AppError, ValidationError
from app.control.model.registry import list_enabled, resolve as resolve_model, get as get_model
from app.control.model.enums import Capability
from .dto import ChatCompletionRequest, ImageGenerationRequest, ImageEditRequest, VideoConfig, ImageConfig
from .chat import completions as chat_completions
from .response import make_chat_response

router = APIRouter(prefix="/v1", dependencies=[Depends(verify_api_key)])


# ---------------------------------------------------------------------------
# /v1/models
# ---------------------------------------------------------------------------

@router.get("/models")
async def list_models():
    import time
    models = [
        {
            "id":       m.model_name,
            "object":   "model",
            "created":  int(time.time()),
            "owned_by": "xai",
            "name":     m.public_name,
        }
        for m in list_enabled()
    ]
    return JSONResponse({"object": "list", "data": models})


@router.get("/models/{model_id}")
async def get_model(model_id: str):
    import time
    from app.control.model.registry import get
    spec = get(model_id)
    if spec is None or not spec.enabled:
        return JSONResponse(
            {"error": {"message": f"Model {model_id!r} not found", "type": "invalid_request_error"}},
            status_code=404,
        )
    return JSONResponse({
        "id":       spec.model_name,
        "object":   "model",
        "created":  int(time.time()),
        "owned_by": "xai",
        "name":     spec.public_name,
    })


# ---------------------------------------------------------------------------
# SSE streaming helpers
# ---------------------------------------------------------------------------

async def _safe_sse(stream: AsyncIterable[str]) -> AsyncGenerator[str, None]:
    """Wrap an SSE stream, converting exceptions to in-band error events."""
    try:
        async for chunk in stream:
            yield chunk
    except AppError as exc:
        payload = orjson.dumps({"error": exc.to_dict()["error"]}).decode()
        yield f"event: error\ndata: {payload}\n\n"
        yield "data: [DONE]\n\n"
    except Exception as exc:
        payload = orjson.dumps({"error": {"message": str(exc), "type": "server_error"}}).decode()
        yield f"event: error\ndata: {payload}\n\n"
        yield "data: [DONE]\n\n"


_SSE_HEADERS = {"Cache-Control": "no-cache", "Connection": "keep-alive"}


# ---------------------------------------------------------------------------
# /v1/chat/completions
# ---------------------------------------------------------------------------

_VALID_ROLES      = {"developer", "system", "user", "assistant", "tool"}
_USER_BLOCK_TYPES = {"text", "image_url", "input_audio", "file"}
_ALLOWED_SIZES    = {"1280x720", "720x1280", "1792x1024", "1024x1792", "1024x1024"}
_EFFORT_VALUES    = {"none", "minimal", "low", "medium", "high", "xhigh"}


def _validate_chat(req: ChatCompletionRequest) -> None:
    from app.platform.errors import ValidationError
    spec = resolve_model(req.model)
    if spec is None or not spec.enabled:
        raise ValidationError(
            f"Model {req.model!r} does not exist or you do not have access to it.",
            param="model", code="model_not_found",
        )
    if not req.messages:
        raise ValidationError("messages cannot be empty", param="messages")
    for i, msg in enumerate(req.messages):
        if msg.role not in _VALID_ROLES:
            raise ValidationError(
                f"role must be one of {sorted(_VALID_ROLES)}",
                param=f"messages.{i}.role",
            )
    if req.temperature is not None and not (0 <= req.temperature <= 2):
        raise ValidationError("temperature must be between 0 and 2", param="temperature")
    if req.top_p is not None and not (0 <= req.top_p <= 1):
        raise ValidationError("top_p must be between 0 and 1", param="top_p")
    if req.reasoning_effort is not None and req.reasoning_effort not in _EFFORT_VALUES:
        raise ValidationError(
            f"reasoning_effort must be one of {sorted(_EFFORT_VALUES)}",
            param="reasoning_effort",
        )


@router.post("/chat/completions")
async def chat_completions_endpoint(req: ChatCompletionRequest):
    _validate_chat(req)

    spec     = resolve_model(req.model)
    messages = [m.model_dump(exclude_none=True) for m in req.messages]

    try:
        # Dispatch by model capability.
        if spec.is_image_edit():
            from .image_edit import edit as img_edit
            cfg    = req.image_config or ImageConfig()
            result = await img_edit(
                model           = req.model,
                messages        = messages,
                n               = cfg.n or 1,
                size            = cfg.size or "1024x1024",
                response_format = cfg.response_format or "url",
                stream          = bool(req.stream),
                chat_format     = True,
            )

        elif spec.is_image():
            from .image import generate as img_gen, resolve_aspect_ratio
            cfg   = req.image_config or ImageConfig()
            size  = cfg.size or "1024x1024"
            fmt   = cfg.response_format or "url"
            n     = cfg.n or 1
            # Extract prompt from last user message.
            prompt = next(
                (m.content for m in reversed(req.messages)
                 if m.role == "user" and isinstance(m.content, str) and m.content.strip()),
                "",
            )
            result = await img_gen(
                model           = req.model,
                prompt          = prompt or "",
                n               = n,
                size            = size,
                response_format = fmt,
                stream          = bool(req.stream),
                chat_format     = True,
            )

        elif spec.is_video():
            from .video import completions as vid_comp
            vcfg = req.video_config or VideoConfig()
            result = await vid_comp(
                model        = req.model,
                messages     = messages,
                stream       = req.stream,
                aspect_ratio = vcfg.aspect_ratio or "3:2",
                video_length = vcfg.video_length or 6,
                resolution   = vcfg.resolution_name or "480p",
                preset       = vcfg.preset or "custom",
            )

        else:
            result = await chat_completions(
                model       = req.model,
                messages    = messages,
                stream      = req.stream,
                tools       = req.tools,
                tool_choice = req.tool_choice,
                temperature = req.temperature or 0.8,
                top_p       = req.top_p or 0.95,
            )

    except AppError:
        raise
    except Exception as exc:
        if req.stream is not False:
            async def _err_stream():
                payload = orjson.dumps({"error": {"message": str(exc), "type": "server_error"}}).decode()
                yield f"event: error\ndata: {payload}\n\n"
                yield "data: [DONE]\n\n"
            return StreamingResponse(_err_stream(), media_type="text/event-stream", headers=_SSE_HEADERS)
        raise

    if isinstance(result, dict):
        return JSONResponse(result)
    return StreamingResponse(_safe_sse(result), media_type="text/event-stream", headers=_SSE_HEADERS)


# ---------------------------------------------------------------------------
# /v1/images/generations (standalone image endpoint)
# ---------------------------------------------------------------------------

@router.post("/images/generations")
async def image_generations(req: ImageGenerationRequest):
    spec = get_model(req.model)
    if spec is None or not spec.enabled or not spec.is_image():
        raise ValidationError(f"Model {req.model!r} is not an image model", param="model")

    from .image import generate as img_gen
    result = await img_gen(
        model           = req.model,
        prompt          = req.prompt,
        n               = req.n or 1,
        size            = req.size or "1024x1024",
        response_format = req.response_format or "url",
        stream          = False,
        chat_format     = False,
    )
    return JSONResponse(result)


# ---------------------------------------------------------------------------
# /v1/images/edits (standalone image-edit endpoint)
# ---------------------------------------------------------------------------

@router.post("/images/edits")
async def image_edits(req: ImageEditRequest):
    spec = get_model(req.model)
    if spec is None or not spec.enabled or not spec.is_image_edit():
        raise ValidationError(f"Model {req.model!r} is not an image-edit model", param="model")

    from .image_edit import edit as img_edit
    # Wrap input into a single-message conversation.
    messages = [{"role": "user", "content": [
        {"type": "text",      "text":      req.prompt},
        {"type": "image_url", "image_url": {"url": req.image}},
    ]}]
    result = await img_edit(
        model           = req.model,
        messages        = messages,
        n               = req.n or 1,
        size            = req.size or "1024x1024",
        response_format = req.response_format or "url",
        stream          = False,
        chat_format     = False,
    )
    return JSONResponse(result)


__all__ = ["router"]
