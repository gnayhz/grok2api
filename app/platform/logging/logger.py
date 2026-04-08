"""Application logger — loguru with structured-field support."""

import os
import sys
from pathlib import Path
from typing import Any

from loguru import logger as _loguru_logger

# Re-export as the canonical logger so imports stay uniform.
logger = _loguru_logger

_configured = False


def setup_logging(
    *,
    level: str = "INFO",
    json_console: bool = False,
    file_logging: bool = True,
    log_dir: Path | None = None,
    max_file_size_mb: int = 100,
    max_files: int = 7,
) -> None:
    """Configure loguru sinks.  Safe to call multiple times (idempotent)."""
    global _configured

    logger.remove()

    fmt_text = (
        "<green>{time:YYYY-MM-DD HH:mm:ss.SSS}</green> | "
        "<level>{level: <8}</level> | "
        "<cyan>{name}</cyan>:<cyan>{line}</cyan> - "
        "<level>{message}</level>"
    )
    fmt_json = "{time} | {level} | {name}:{line} | {message}"

    logger.add(
        sys.stdout,
        level=level.upper(),
        format=fmt_json if json_console else fmt_text,
        colorize=not json_console,
        enqueue=True,
    )

    if file_logging:
        _dir = log_dir or (Path.cwd() / "logs")
        _dir.mkdir(parents=True, exist_ok=True)
        rotation = f"{max_file_size_mb} MB" if max_file_size_mb > 0 else None
        retention = max_files if max_files > 0 else None
        logger.add(
            str(_dir / "app.log"),
            level="DEBUG",
            format=fmt_text,
            rotation=rotation,
            retention=retention,
            enqueue=True,
            encoding="utf-8",
        )

    _configured = True


def reload_logging(
    *,
    default_level: str = "INFO",
    json_console: bool = False,
    max_file_size_mb: int = 100,
    max_files: int = 7,
) -> None:
    """Re-configure logging from runtime values (called after config loads)."""
    level = os.getenv("LOG_LEVEL", default_level)
    setup_logging(
        level=level,
        json_console=json_console,
        file_logging=True,
        max_file_size_mb=max_file_size_mb,
        max_files=max_files,
    )


__all__ = ["logger", "setup_logging", "reload_logging"]
