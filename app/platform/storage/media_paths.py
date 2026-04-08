"""Shared local media storage paths."""

from pathlib import Path

_FILES_DIR = Path("data/files")
_IMAGE_DIR = _FILES_DIR / "images"
_VIDEO_DIR = _FILES_DIR / "videos"


def image_files_dir() -> Path:
    """Return the local image storage directory."""
    _IMAGE_DIR.mkdir(parents=True, exist_ok=True)
    return _IMAGE_DIR


def video_files_dir() -> Path:
    """Return the local video storage directory."""
    _VIDEO_DIR.mkdir(parents=True, exist_ok=True)
    return _VIDEO_DIR


__all__ = ["image_files_dir", "video_files_dir"]
