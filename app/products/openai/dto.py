"""OpenAI-compatible request/response DTOs."""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union

from pydantic import BaseModel, Field


class MessageItem(BaseModel):
    role:         str
    content:      Optional[Union[str, List[Dict[str, Any]]]] = None
    tool_calls:   Optional[List[Dict[str, Any]]]             = None
    tool_call_id: Optional[str]                              = None
    name:         Optional[str]                              = None


class ImageConfig(BaseModel):
    n:               Optional[int] = Field(1, ge=1, le=10)
    size:            Optional[str] = "1024x1024"
    response_format: Optional[str] = None


class VideoConfig(BaseModel):
    aspect_ratio:    Optional[str] = "3:2"
    video_length:    Optional[int] = Field(6, ge=6, le=30)
    resolution_name: Optional[str] = "480p"
    preset:          Optional[str] = "custom"


class ChatCompletionRequest(BaseModel):
    model:              str
    messages:           List[MessageItem]
    stream:             Optional[bool]                   = None
    reasoning_effort:   Optional[str]                    = None
    temperature:        Optional[float]                  = 0.8
    top_p:              Optional[float]                  = 0.95
    image_config:       Optional[ImageConfig]            = None
    video_config:       Optional[VideoConfig]            = None
    tools:              Optional[List[Dict[str, Any]]]   = None
    tool_choice:        Optional[Union[str, Dict[str, Any]]] = None
    parallel_tool_calls: Optional[bool]                 = True
    max_tokens:         Optional[int]                    = None


class ImageGenerationRequest(BaseModel):
    model:           str
    prompt:          str
    n:               Optional[int] = Field(1, ge=1, le=10)
    size:            Optional[str] = "1024x1024"
    response_format: Optional[str] = "url"


class ImageEditRequest(BaseModel):
    """OpenAI /v1/images/edits — edit an existing image with a prompt."""

    model:           str
    prompt:          str
    image:           str                # URL or data-URI of the source image
    mask:            Optional[str] = None  # ignored (not supported upstream)
    n:               Optional[int] = Field(1, ge=1, le=10)
    size:            Optional[str] = "1024x1024"
    response_format: Optional[str] = "url"


__all__ = [
    "MessageItem", "ImageConfig", "VideoConfig",
    "ChatCompletionRequest", "ImageGenerationRequest", "ImageEditRequest",
]
