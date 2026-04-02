"""
Unified session helpers for the proxy domain.
"""

from __future__ import annotations

from typing import Any

from curl_cffi.const import CurlOpt

from app.services.proxy.config import load_proxy_domain_config
from app.services.proxy.models import ProxyLease


def build_http_proxies(proxy_url: str) -> dict[str, str] | None:
    if not proxy_url:
        return None
    return {"http": proxy_url, "https": proxy_url}


def resolve_browser(
    *,
    lease: ProxyLease | None = None,
    browser_override: str | None = None,
) -> str:
    if lease is not None and lease.browser:
        return lease.browser
    if browser_override:
        return browser_override
    return load_proxy_domain_config().clearance.browser


def should_skip_proxy_ssl(
    *,
    proxy_url: str = "",
    lease: ProxyLease | None = None,
) -> bool:
    config = load_proxy_domain_config()
    effective_proxy = proxy_url or (lease.proxy_url if lease is not None else "")
    return bool(config.skip_proxy_ssl_verify) and bool(effective_proxy)


def build_curl_options(
    *,
    proxy_url: str = "",
    lease: ProxyLease | None = None,
    base: dict[Any, Any] | None = None,
) -> dict[Any, Any]:
    options = dict(base or {})
    if should_skip_proxy_ssl(proxy_url=proxy_url, lease=lease):
        options[CurlOpt.PROXY_SSL_VERIFYPEER] = 0
        options[CurlOpt.PROXY_SSL_VERIFYHOST] = 0
    return options


def build_session_kwargs(
    *,
    lease: ProxyLease | None = None,
    browser_override: str | None = None,
    kwargs: dict[str, Any] | None = None,
) -> dict[str, Any]:
    session_kwargs = dict(kwargs or {})
    if not session_kwargs.get("impersonate"):
        browser = resolve_browser(lease=lease, browser_override=browser_override)
        if browser:
            session_kwargs["impersonate"] = browser
    proxy_url = session_kwargs.get("proxy", "") or ""
    if not proxy_url:
        proxies = session_kwargs.get("proxies")
        if isinstance(proxies, dict):
            proxy_url = str(proxies.get("https") or proxies.get("http") or "")
    curl_options = build_curl_options(
        proxy_url=proxy_url,
        lease=lease,
        base=session_kwargs.get("curl_options"),
    )
    if curl_options:
        session_kwargs["curl_options"] = curl_options
    return session_kwargs


__all__ = [
    "build_http_proxies",
    "build_curl_options",
    "build_session_kwargs",
    "resolve_browser",
    "should_skip_proxy_ssl",
]
