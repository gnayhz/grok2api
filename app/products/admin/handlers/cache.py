"""Local cache management — stats, list, clear, delete."""

from __future__ import annotations

from pathlib import Path
from typing import Any, Dict

from fastapi import APIRouter, HTTPException, Query

router = APIRouter(prefix="/cache")

# ---------------------------------------------------------------------------
# Lightweight local cache service (inlined to avoid external dep)
# ---------------------------------------------------------------------------

_BASE = Path(__file__).resolve().parents[4] / "data" / "tmp"
_IMAGE_EXTS = {".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp"}
_VIDEO_EXTS = {".mp4", ".mov", ".m4v", ".webm", ".avi", ".mkv"}


def _dir(media_type: str) -> Path:
    d = _BASE / ("image" if media_type == "image" else "video")
    d.mkdir(parents=True, exist_ok=True)
    return d


def _exts(media_type: str):
    return _IMAGE_EXTS if media_type == "image" else _VIDEO_EXTS


def _stats(media_type: str) -> Dict[str, Any]:
    d = _dir(media_type)
    if not d.exists():
        return {"count": 0, "size_mb": 0.0}
    allowed = _exts(media_type)
    files = [f for f in d.glob("*") if f.is_file() and f.suffix.lower() in allowed]
    total_size = sum(f.stat().st_size for f in files)
    return {"count": len(files), "size_mb": round(total_size / 1024 / 1024, 2)}


def _list_files(media_type: str, page: int, page_size: int) -> Dict[str, Any]:
    d = _dir(media_type)
    if not d.exists():
        return {"total": 0, "page": page, "page_size": page_size, "items": []}
    allowed = _exts(media_type)
    files = sorted(
        (f for f in d.glob("*") if f.is_file() and f.suffix.lower() in allowed),
        key=lambda f: f.stat().st_mtime,
        reverse=True,
    )
    total = len(files)
    start = (page - 1) * page_size
    chunk = files[start : start + page_size]
    items = []
    for f in chunk:
        st = f.stat()
        items.append({
            "name": f.name,
            "size_bytes": st.st_size,
            "modified_at": st.st_mtime,
        })
    return {"total": total, "page": page, "page_size": page_size, "items": items}


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.get("")
async def cache_stats():
    return {
        "local_image": _stats("image"),
        "local_video": _stats("video"),
    }


@router.get("/list")
async def list_local(
    cache_type: str = "image",
    type_: str = Query(default=None, alias="type"),
    page: int = 1,
    page_size: int = 1000,
):
    if type_:
        cache_type = type_
    return {"status": "success", **_list_files(cache_type, page, page_size)}


@router.post("/clear")
async def clear_local(data: dict):
    media_type = data.get("type", "image")
    d = _dir(media_type)
    allowed = _exts(media_type)
    removed = 0
    for f in d.glob("*"):
        if f.is_file() and f.suffix.lower() in allowed:
            f.unlink(missing_ok=True)
            removed += 1
    return {"status": "success", "result": {"removed": removed}}


@router.post("/item/delete")
async def delete_local_item(data: dict):
    media_type = data.get("type", "image")
    name = data.get("name")
    if not name:
        raise HTTPException(status_code=400, detail="Missing file name")
    target = _dir(media_type) / name
    if not target.is_file():
        raise HTTPException(status_code=404, detail="File not found")
    target.unlink(missing_ok=True)
    return {"status": "success", "result": {"deleted": name}}
