"""
Migration helpers for legacy config layout.
"""

from __future__ import annotations

from copy import deepcopy
from typing import Any

from app.core.logger import logger


def migrate_deprecated_config(
    config: dict[str, Any], valid_sections: set[str]
) -> tuple[dict[str, Any], set[str]]:
    """
    Migrate deprecated config sections into the current config layout.

    Returns:
        (migrated_config, deprecated_sections)
    """
    migration_map: dict[str, str | list[str]] = {
        "grok.temporary": "app.temporary",
        "grok.disable_memory": "app.disable_memory",
        "grok.stream": "app.stream",
        "grok.thinking": "app.thinking",
        "grok.dynamic_statsig": "app.dynamic_statsig",
        "grok.filter_tags": "app.filter_tags",
        "grok.timeout": "voice.timeout",
        "grok.base_proxy_url": "proxy.base_proxy_url",
        "grok.asset_proxy_url": "proxy.asset_proxy_url",
        "network.base_proxy_url": "proxy.base_proxy_url",
        "network.asset_proxy_url": "proxy.asset_proxy_url",
        "grok.cf_clearance": "proxy.cf_clearance",
        "grok.browser": "proxy.browser",
        "grok.user_agent": "proxy.user_agent",
        "security.cf_clearance": "proxy.cf_clearance",
        "security.browser": "proxy.browser",
        "security.user_agent": "proxy.user_agent",
        "grok.max_retry": "retry.max_retry",
        "grok.retry_status_codes": "retry.retry_status_codes",
        "grok.retry_backoff_base": "retry.retry_backoff_base",
        "grok.retry_backoff_factor": "retry.retry_backoff_factor",
        "grok.retry_backoff_max": "retry.retry_backoff_max",
        "grok.retry_budget": "retry.retry_budget",
        "grok.video_idle_timeout": "video.stream_timeout",
        "grok.image_ws_nsfw": "image.nsfw",
        "grok.image_ws_blocked_seconds": "image.final_timeout",
        "grok.image_ws_final_min_bytes": "image.final_min_bytes",
        "grok.image_ws_medium_min_bytes": "image.medium_min_bytes",
        "network.timeout": [
            "chat.timeout",
            "image.timeout",
            "video.timeout",
            "voice.timeout",
        ],
        "timeout.stream_idle_timeout": [
            "chat.stream_timeout",
            "image.stream_timeout",
            "video.stream_timeout",
        ],
        "timeout.video_idle_timeout": "video.stream_timeout",
        "image.image_ws_nsfw": "image.nsfw",
        "image.image_ws_blocked_seconds": "image.final_timeout",
        "image.image_ws_final_min_bytes": "image.final_min_bytes",
        "image.image_ws_medium_min_bytes": "image.medium_min_bytes",
        "performance.assets_max_concurrent": [
            "asset.upload_concurrent",
            "asset.download_concurrent",
            "asset.list_concurrent",
            "asset.delete_concurrent",
        ],
        "performance.assets_delete_batch_size": "asset.delete_batch_size",
        "performance.assets_batch_size": "asset.list_batch_size",
        "performance.media_max_concurrent": ["chat.concurrent", "video.concurrent"],
        "performance.usage_max_concurrent": "usage.concurrent",
        "performance.usage_batch_size": "usage.batch_size",
        "performance.nsfw_max_concurrent": "nsfw.concurrent",
        "performance.nsfw_batch_size": "nsfw.batch_size",
    }

    deprecated_sections = set(config.keys()) - valid_sections
    if not deprecated_sections:
        return config, set()

    result = {k: deepcopy(v) for k, v in config.items() if k in valid_sections}
    migrated_count = 0

    for old_section, old_values in config.items():
        if not isinstance(old_values, dict):
            continue
        for old_key, old_value in old_values.items():
            old_path = f"{old_section}.{old_key}"
            new_paths = migration_map.get(old_path)
            if not new_paths:
                continue
            if isinstance(new_paths, str):
                new_paths = [new_paths]
            for new_path in new_paths:
                try:
                    new_section, new_key = new_path.split(".", 1)
                    if new_section not in result:
                        result[new_section] = {}
                    if new_key not in result[new_section]:
                        result[new_section][new_key] = old_value
                    migrated_count += 1
                    logger.debug(
                        "Migrated config: {} -> {} = {}",
                        old_path,
                        new_path,
                        old_value,
                    )
                except Exception as error:
                    logger.warning(
                        "Skip config migration for {}: {}",
                        old_path,
                        error,
                    )
                    continue
            if isinstance(result.get(old_section), dict):
                result[old_section].pop(old_key, None)

    legacy_chat_map = {
        "temporary": "temporary",
        "disable_memory": "disable_memory",
        "stream": "stream",
        "thinking": "thinking",
        "dynamic_statsig": "dynamic_statsig",
        "filter_tags": "filter_tags",
    }
    chat_section = config.get("chat")
    if isinstance(chat_section, dict):
        app_section = result.setdefault("app", {})
        for old_key, new_key in legacy_chat_map.items():
            if old_key in chat_section and new_key not in app_section:
                app_section[new_key] = chat_section[old_key]
                if isinstance(result.get("chat"), dict):
                    result["chat"].pop(old_key, None)
                migrated_count += 1
                logger.debug(
                    "Migrated config: chat.{} -> app.{} = {}",
                    old_key,
                    new_key,
                    chat_section[old_key],
                )

    if migrated_count > 0:
        logger.info(
            "Migrated {} config items from deprecated/legacy sections",
            migrated_count,
        )

    return result, deprecated_sections


__all__ = ["migrate_deprecated_config"]
