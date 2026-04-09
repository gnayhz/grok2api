"""Typed configuration snapshot — built once at startup, read-only at runtime."""

import asyncio
import os
from pathlib import Path
from typing import Any

import tomli_w

from .loader import get_nested, load_config

_BASE_DIR = Path(__file__).resolve().parents[3]  # project root
_DATA_DIR = _BASE_DIR / "data"


def _resolve_defaults_path() -> Path:
    return _BASE_DIR / "config.defaults.toml"


def _resolve_user_path() -> Path:
    return _DATA_DIR / "config.toml"


def _mtime(path: Path) -> float:
    """Return file mtime, or 0.0 if the file does not exist."""
    try:
        return os.stat(path).st_mtime
    except OSError:
        return 0.0


class ConfigSnapshot:
    """Immutable view over the loaded configuration dict.

    Reloads from disk only when a config file's mtime changes, so the
    per-request overhead is a pair of stat() syscalls rather than full
    TOML parsing on every request.
    """

    def __init__(self) -> None:
        self._data: dict[str, Any] = {}
        self._loaded = False
        self._lock = asyncio.Lock()
        self._mtime_defaults: float = 0.0
        self._mtime_user: float = 0.0

    async def load(
        self,
        defaults_path: Path | None = None,
        user_path: Path | None = None,
    ) -> None:
        """Reload config if any source file changed since last load.

        Safe to call on every request — skips disk I/O when nothing changed.
        Pass explicit paths only during testing; production uses the defaults.
        """
        dp = defaults_path or _resolve_defaults_path()
        up = user_path or _resolve_user_path()

        mt_dp = _mtime(dp)
        mt_up = _mtime(up)

        # Fast path: files unchanged — skip lock and disk I/O entirely.
        if self._loaded and mt_dp == self._mtime_defaults and mt_up == self._mtime_user:
            return

        async with self._lock:
            # Re-check under lock to avoid redundant reloads under concurrency.
            mt_dp = _mtime(dp)
            mt_up = _mtime(up)
            if self._loaded and mt_dp == self._mtime_defaults and mt_up == self._mtime_user:
                return

            if not dp.exists():
                raise RuntimeError(f"Missing required defaults config: {dp}")
            self._data = await asyncio.to_thread(load_config, dp, up)
            self._loaded = True
            self._mtime_defaults = mt_dp
            self._mtime_user = mt_up

    async def ensure_loaded(self) -> None:
        """Load with defaults if not yet loaded."""
        if not self._loaded:
            await self.load()

    def get(self, key: str, default: Any = None) -> Any:
        """Retrieve a value by dotted key (e.g. ``"account.refresh.enabled"``)."""
        return get_nested(self._data, key, default)

    def get_int(self, key: str, default: int = 0) -> int:
        val = self.get(key, default)
        try:
            return int(val)
        except (TypeError, ValueError):
            return default

    def get_float(self, key: str, default: float = 0.0) -> float:
        val = self.get(key, default)
        try:
            return float(val)
        except (TypeError, ValueError):
            return default

    def get_bool(self, key: str, default: bool = False) -> bool:
        val = self.get(key, default)
        if isinstance(val, bool):
            return val
        if isinstance(val, str):
            return val.strip().lower() in {"1", "true", "yes", "on"}
        return bool(val)

    def get_str(self, key: str, default: str = "") -> str:
        val = self.get(key, default)
        return str(val) if val is not None else default

    def get_list(self, key: str, default: list | None = None) -> list:
        val = self.get(key, default)
        if val is None:
            return [] if default is None else default
        if isinstance(val, list):
            return val
        if isinstance(val, str):
            return [p.strip() for p in val.split(",") if p.strip()]
        return [val]

    async def update(self, patch: dict[str, Any]) -> None:
        """Merge *patch* into the current config and persist to user config file."""
        async with self._lock:
            from .loader import _deep_merge
            self._data = _deep_merge(self._data, patch)
            user_path = _resolve_user_path()
            await asyncio.to_thread(self._write_toml, user_path, self._data)
            # Invalidate cached mtime so the next load() call re-reads the file
            # and other workers pick up the change on their next request.
            self._mtime_user = 0.0

    @staticmethod
    def _write_toml(path: Path, data: dict[str, Any]) -> None:
        """Write config dict to TOML file."""
        path.parent.mkdir(parents=True, exist_ok=True)
        with open(path, "wb") as fh:
            tomli_w.dump(data, fh)

    def raw(self) -> dict[str, Any]:
        """Return a shallow copy of the underlying data dict."""
        return dict(self._data)


# Module-level singleton — imported everywhere.
config = ConfigSnapshot()


def get_config(key: str | None = None, default: Any = None) -> Any:
    """Convenience wrapper around the module-level ``config`` singleton.

    Without arguments, returns the ``ConfigSnapshot`` instance itself.
    With a *key*, returns the config value at that dotted path.
    """
    if key is None:
        return config
    return config.get(key, default)


__all__ = ["ConfigSnapshot", "config", "get_config"]
