"""
Config normalization for the unified proxy / clearance domain.
"""

from __future__ import annotations

from typing import Optional

from pydantic import BaseModel, Field

from app.core.config import get_config
from app.services.proxy.models import ClearanceMode, EgressMode, ProxyScope


def _parse_proxy_urls(raw: object) -> list[str]:
    if raw is None:
        return []
    if isinstance(raw, str):
        return [item.strip() for item in raw.split(",") if item.strip()]
    if isinstance(raw, (list, tuple, set)):
        return [str(item).strip() for item in raw if str(item).strip()]
    return []


def _detect_egress_mode(urls: list[str]) -> EgressMode:
    if not urls:
        return EgressMode.DIRECT
    if len(urls) == 1:
        return EgressMode.SINGLE_PROXY
    return EgressMode.PROXY_POOL


def _to_bool(value: object, default: bool = False) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"1", "true", "yes", "on"}:
            return True
        if lowered in {"0", "false", "no", "off"}:
            return False
    return default if value is None else bool(value)


def _to_int(value: object, default: int, minimum: int) -> int:
    try:
        return max(minimum, int(value))
    except (TypeError, ValueError):
        return max(minimum, default)


class ProxyScopeConfig(BaseModel):
    scope: ProxyScope
    egress_mode: EgressMode
    urls: list[str] = Field(default_factory=list)


class ProxyClearanceConfig(BaseModel):
    mode: ClearanceMode
    cf_cookies: str = ""
    cf_clearance: str = ""
    user_agent: str = ""
    browser: str = ""
    flaresolverr_url: str = ""
    refresh_interval_sec: int = Field(default=600, ge=60)
    timeout_sec: int = Field(default=60, ge=1)


class ProxyDomainConfig(BaseModel):
    app: ProxyScopeConfig
    asset: ProxyScopeConfig
    clearance: ProxyClearanceConfig
    skip_proxy_ssl_verify: bool = False

    def cache_key(self) -> tuple:
        return (
            self.app.egress_mode.value,
            tuple(self.app.urls),
            self.asset.egress_mode.value,
            tuple(self.asset.urls),
            self.clearance.mode.value,
            self.clearance.cf_cookies,
            self.clearance.cf_clearance,
            self.clearance.user_agent,
            self.clearance.browser,
            self.clearance.flaresolverr_url,
            self.clearance.refresh_interval_sec,
            self.clearance.timeout_sec,
            self.skip_proxy_ssl_verify,
        )


def _resolve_scope(scope: ProxyScope, key: str) -> ProxyScopeConfig:
    urls = _parse_proxy_urls(get_config(key, ""))
    return ProxyScopeConfig(
        scope=scope,
        egress_mode=_detect_egress_mode(urls),
        urls=urls,
    )


def _resolve_clearance_mode(
    *,
    flaresolverr_url: str,
    auto_enabled: bool,
    cf_cookies: str,
    cf_clearance: str,
) -> ClearanceMode:
    if auto_enabled and flaresolverr_url:
        return ClearanceMode.MANAGED
    if cf_cookies or cf_clearance:
        return ClearanceMode.MANUAL
    return ClearanceMode.NONE


def load_proxy_domain_config() -> ProxyDomainConfig:
    flaresolverr_url = str(get_config("proxy.flaresolverr_url", "") or "").strip()
    cf_cookies = str(get_config("proxy.cf_cookies", "") or "").strip()
    cf_clearance = str(get_config("proxy.cf_clearance", "") or "").strip()
    user_agent = str(get_config("proxy.user_agent", "") or "").strip()
    browser = str(get_config("proxy.browser", "") or "").strip()
    auto_enabled = _to_bool(get_config("proxy.enabled", False), False)

    return ProxyDomainConfig(
        app=_resolve_scope(ProxyScope.APP, "proxy.base_proxy_url"),
        asset=_resolve_scope(ProxyScope.ASSET, "proxy.asset_proxy_url"),
        clearance=ProxyClearanceConfig(
            mode=_resolve_clearance_mode(
                flaresolverr_url=flaresolverr_url,
                auto_enabled=auto_enabled,
                cf_cookies=cf_cookies,
                cf_clearance=cf_clearance,
            ),
            cf_cookies=cf_cookies,
            cf_clearance=cf_clearance,
            user_agent=user_agent,
            browser=browser,
            flaresolverr_url=flaresolverr_url,
            refresh_interval_sec=_to_int(
                get_config("proxy.refresh_interval", 600),
                600,
                60,
            ),
            timeout_sec=_to_int(
                get_config("proxy.timeout", 60),
                60,
                1,
            ),
        ),
        skip_proxy_ssl_verify=_to_bool(
            get_config("proxy.skip_proxy_ssl_verify", False),
            False,
        ),
    )


def get_scope_config(
    config: ProxyDomainConfig,
    scope: ProxyScope,
) -> ProxyScopeConfig:
    return config.asset if scope == ProxyScope.ASSET else config.app


def egress_affinity_key(
    scope: ProxyScope,
    proxy_url: str,
    *,
    request_kind: Optional[str] = None,
) -> str:
    if not proxy_url:
        base = f"{scope.value}:direct"
    else:
        base = f"{scope.value}:{proxy_url}"
    if request_kind:
        return f"{base}:{request_kind}"
    return base


__all__ = [
    "ProxyClearanceConfig",
    "ProxyDomainConfig",
    "ProxyScopeConfig",
    "egress_affinity_key",
    "get_scope_config",
    "load_proxy_domain_config",
]
