"""Typed configuration snapshot — built once at startup, read-only at runtime."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

from .loader import get_nested, load_config

_BASE_DIR = Path(__file__).resolve().parents[3]  # project root


class ConfigSnapshot:
    """Immutable view over the loaded configuration dict.

    Call ``load()`` once during application startup.  All subsequent reads
    via ``get()`` are lock-free and non-blocking.
    """

    def __init__(self) -> None:
        self._data: dict[str, Any] = {}
        self._loaded = False
        self._lock = asyncio.Lock()

    async def load(
        self,
        defaults_path: Path | None = None,
        user_path: Path | None = None,
    ) -> None:
        """Load configuration files.  Idempotent — subsequent calls reload."""
        async with self._lock:
            dp = defaults_path or (_BASE_DIR / "config.defaults.toml")
            up = user_path or (_BASE_DIR / "config.toml")
            self._data = await asyncio.to_thread(load_config, dp, up)
            self._loaded = True

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
            # Persist to user config file.
            user_path = _BASE_DIR / "config.toml"
            await asyncio.to_thread(self._write_toml, user_path, self._data)

    @staticmethod
    def _write_toml(path: Path, data: dict[str, Any]) -> None:
        """Write config dict to TOML file."""
        try:
            import tomli_w
            with open(path, "wb") as fh:
                tomli_w.dump(data, fh)
        except ImportError:
            # Fallback: write as minimal TOML manually.
            import json
            with open(path, "w") as fh:
                for section, values in data.items():
                    if isinstance(values, dict):
                        fh.write(f"\n[{section}]\n")
                        for k, v in values.items():
                            if isinstance(v, bool):
                                fh.write(f"{k} = {'true' if v else 'false'}\n")
                            elif isinstance(v, (int, float)):
                                fh.write(f"{k} = {v}\n")
                            elif isinstance(v, str):
                                fh.write(f'{k} = {json.dumps(v)}\n')
                            elif isinstance(v, list):
                                fh.write(f"{k} = {json.dumps(v)}\n")
                            else:
                                fh.write(f'{k} = {json.dumps(v)}\n')

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
