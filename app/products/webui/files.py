"""Local cached image/video file serving."""

from __future__ import annotations

from pathlib import Path

import aiofiles.os
from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse

from app.platform.logging.logger import logger

router = APIRouter()

_BASE = Path(__file__).resolve().parents[3] / "data" / "tmp"
_IMAGE_DIR = _BASE / "image"
_VIDEO_DIR = _BASE / "video"

_CACHE_HEADERS = {"Cache-Control": "public, max-age=31536000, immutable"}


@router.get("/image/{filename:path}")
async def get_image(filename: str):
    if "/" in filename:
        filename = filename.replace("/", "-")

    file_path = _IMAGE_DIR / filename
    if await aiofiles.os.path.exists(file_path) and await aiofiles.os.path.isfile(file_path):
        suffix = file_path.suffix.lower()
        content_type = {".png": "image/png", ".webp": "image/webp"}.get(suffix, "image/jpeg")
        return FileResponse(file_path, media_type=content_type, headers=_CACHE_HEADERS)

    logger.warning("Image not found: {}", filename)
    raise HTTPException(status_code=404, detail="Image not found")


@router.get("/video/{filename:path}")
async def get_video(filename: str):
    if "/" in filename:
        filename = filename.replace("/", "-")

    file_path = _VIDEO_DIR / filename
    if await aiofiles.os.path.exists(file_path) and await aiofiles.os.path.isfile(file_path):
        return FileResponse(file_path, media_type="video/mp4", headers=_CACHE_HEADERS)

    logger.warning("Video not found: {}", filename)
    raise HTTPException(status_code=404, detail="Video not found")
