"""XAI video creation protocol — payload builders for media/post/create endpoints."""

from __future__ import annotations

from typing import Any

MEDIA_POST_URL    = "https://grok.com/rest/media/post/create"
MEDIA_LINK_URL    = "https://grok.com/rest/media/post/create-link"
VIDEO_UPSCALE_URL = "https://grok.com/rest/media/video/upscale"


def build_media_post_payload(
    *,
    media_type: str,
    media_url:  str  = "",
    prompt:     str  = "",
) -> dict[str, Any]:
    """Build payload for POST /rest/media/post/create."""
    payload: dict[str, Any] = {"mediaType": media_type}
    if media_url:
        payload["mediaUrl"] = media_url
    if prompt:
        payload["prompt"] = prompt
    return payload


def build_video_request_payload(
    *,
    prompt:       str,
    aspect_ratio: str = "3:2",
    video_length: int = 6,
    resolution:   str = "480p",
    preset:       str = "custom",
) -> dict[str, Any]:
    """Build the app-chat request_overrides for video generation."""
    return {
        "mediaType":    "video",
        "aspectRatio":  aspect_ratio,
        "videoDuration": video_length,
        "resolution":   resolution,
        "preset":       preset,
    }


def build_upscale_payload(video_id: str) -> dict[str, Any]:
    return {"videoId": video_id}


def build_media_link_payload(post_id: str) -> dict[str, Any]:
    """Build payload for POST /rest/media/post/create-link."""
    return {
        "postId":   post_id,
        "source":   "post-page",
        "platform": "web",
    }


__all__ = [
    "MEDIA_POST_URL", "MEDIA_LINK_URL", "VIDEO_UPSCALE_URL",
    "build_media_post_payload", "build_video_request_payload",
    "build_upscale_payload", "build_media_link_payload",
]
