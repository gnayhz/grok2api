"""
配置管理

- config.toml: 运行时配置
- config.defaults.toml: 默认配置基线
"""

from copy import deepcopy
import asyncio
from pathlib import Path
from typing import Any, Dict
import tomllib

from app.core.logger import logger
from app.migrate.config import migrate_deprecated_config

DEFAULT_CONFIG_FILE = Path(__file__).parent.parent.parent / "config.defaults.toml"


def _deep_merge(base: Dict[str, Any], override: Dict[str, Any]) -> Dict[str, Any]:
    """深度合并字典: override 覆盖 base."""
    if not isinstance(base, dict):
        return deepcopy(override) if isinstance(override, dict) else deepcopy(base)

    result = deepcopy(base)
    if not isinstance(override, dict):
        return result

    for key, val in override.items():
        if isinstance(val, dict) and isinstance(result.get(key), dict):
            result[key] = _deep_merge(result[key], val)
        else:
            result[key] = val
    return result


def _prune_unknown_config(
    config: Dict[str, Any], defaults: Dict[str, Any]
) -> tuple[Dict[str, Any], Dict[str, Any]]:
    """
    Remove unknown config sections/keys that are not present in defaults.

    Returns:
        (pruned_config, removed_items)
    """
    if not isinstance(config, dict):
        return {}, {"__root__": config}

    pruned: Dict[str, Any] = {}
    removed: Dict[str, Any] = {}

    for section, value in config.items():
        if section not in defaults:
            removed[section] = value
            continue

        default_section = defaults.get(section)
        if isinstance(default_section, dict) and isinstance(value, dict):
            allowed_keys = set(default_section.keys())
            kept = {k: v for k, v in value.items() if k in allowed_keys}
            extra = {k: v for k, v in value.items() if k not in allowed_keys}
            if extra:
                removed[section] = extra
            if kept:
                pruned[section] = kept
        else:
            pruned[section] = value

    return pruned, removed


def _summarize_removed(removed: Dict[str, Any]) -> Dict[str, list]:
    summary: Dict[str, list] = {}
    for section, value in removed.items():
        if isinstance(value, dict):
            summary[section] = list(value.keys())
        else:
            summary[section] = ["<section>"]
    return summary


def _load_defaults() -> Dict[str, Any]:
    """加载默认配置文件"""
    if not DEFAULT_CONFIG_FILE.exists():
        return {}
    try:
        with DEFAULT_CONFIG_FILE.open("rb") as f:
            return tomllib.load(f)
    except Exception as e:
        logger.warning(f"Failed to load defaults from {DEFAULT_CONFIG_FILE}: {e}")
        return {}


class Config:
    """配置管理器"""

    _instance = None
    _config = {}

    def __init__(self):
        self._config = {}
        self._defaults = {}
        self._code_defaults = {}
        self._defaults_loaded = False
        self._loaded = False
        self._load_lock = asyncio.Lock()

    def register_defaults(self, defaults: Dict[str, Any]):
        """注册代码中定义的默认值"""
        self._code_defaults = _deep_merge(self._code_defaults, defaults)

    def _ensure_defaults(self):
        if self._defaults_loaded:
            return
        file_defaults = _load_defaults()
        # 合并文件默认值和代码默认值（代码默认值优先级更低）
        self._defaults = _deep_merge(self._code_defaults, file_defaults)
        self._defaults_loaded = True

    async def load(self):
        """显式加载配置"""
        try:
            from app.core.storage import get_storage, LocalStorage

            self._ensure_defaults()

            storage = get_storage()
            config_data = await storage.load_config()
            from_remote = True

            # 从本地 data/config.toml 初始化后端
            if config_data is None:
                local_storage = LocalStorage()
                from_remote = False
                try:
                    # 尝试读取本地配置
                    config_data = await local_storage.load_config()
                except Exception as e:
                    logger.info(f"Failed to auto-init config from local: {e}")
                    config_data = {}

            config_data = config_data or {}

            # 检查是否有废弃的配置节
            valid_sections = set(self._defaults.keys())
            config_data, deprecated_sections = migrate_deprecated_config(
                config_data, valid_sections
            )
            if deprecated_sections:
                logger.info(
                    f"Cleaned deprecated config sections: {deprecated_sections}"
                )

            config_data, removed_items = _prune_unknown_config(
                config_data, self._defaults
            )
            if removed_items:
                logger.info(
                    "Removed unknown config items: {}",
                    _summarize_removed(removed_items),
                )

            merged = _deep_merge(self._defaults, config_data)

            # 自动回填缺失配置到存储
            # 或迁移了配置后需要更新
            # 保护：当远程存储返回 None 且本地也没有可迁移配置时，不覆盖远程配置，避免误重置。
            has_local_seed = bool(config_data)
            allow_bootstrap_empty_remote = (
                (not from_remote) and has_local_seed
            )
            should_persist = (
                allow_bootstrap_empty_remote
                or (merged != config_data and bool(config_data))
                or deprecated_sections
                or removed_items
            )
            if should_persist:
                async with storage.acquire_lock("config_save", timeout=10):
                    await storage.save_config(merged)
                if not from_remote and has_local_seed:
                    logger.info(
                        f"Initialized remote storage ({storage.__class__.__name__}) with config baseline."
                    )
                if deprecated_sections:
                    logger.info("Configuration automatically migrated and cleaned.")
            elif not from_remote and not has_local_seed:
                logger.warning(
                    "Skip persisting defaults: empty config source detected, keep runtime merged config only."
                )

            self._config = merged
            self._loaded = True
        except Exception as e:
            logger.error(f"Error loading config: {e}")
            self._config = {}
            self._loaded = False

    async def ensure_loaded(self):
        """确保配置至少成功加载一次（按需懒加载，线程安全）"""
        if self._loaded:
            return
        async with self._load_lock:
            if self._loaded:
                return
            await self.load()

    def get(self, key: str, default: Any = None) -> Any:
        """
        获取配置值

        Args:
            key: 配置键，格式 "section.key"
            default: 默认值
        """
        if "." in key:
            try:
                section, attr = key.split(".", 1)
                return self._config.get(section, {}).get(attr, default)
            except (ValueError, AttributeError):
                return default

        return self._config.get(key, default)

    async def update(self, new_config: dict):
        """更新配置"""
        from app.core.storage import get_storage

        storage = get_storage()
        async with storage.acquire_lock("config_save", timeout=10):
            self._ensure_defaults()
            base = _deep_merge(self._defaults, self._config or {})
            merged = _deep_merge(base, new_config or {})
            merged, removed_items = _prune_unknown_config(merged, self._defaults)
            if removed_items:
                logger.info(
                    "Removed unknown config items on update: {}",
                    _summarize_removed(removed_items),
                )
            await storage.save_config(merged)
            self._config = merged


# 全局配置实例
config = Config()


def get_config(key: str, default: Any = None) -> Any:
    """获取配置"""
    return config.get(key, default)


def register_defaults(defaults: Dict[str, Any]):
    """注册默认配置"""
    config.register_defaults(defaults)


__all__ = ["Config", "config", "get_config", "register_defaults"]
