"""Shared header builders for reverse interfaces."""

from typing import Dict, Optional

from app.services.proxy.headers import (
    build_http_headers,
    build_sso_cookie,
    build_ws_headers as build_proxy_ws_headers,
)
from app.services.proxy.models import ProxyLease


def build_ws_headers(
    token: Optional[str] = None,
    origin: Optional[str] = None,
    extra: Optional[Dict[str, str]] = None,
    *,
    lease: ProxyLease | None = None,
) -> Dict[str, str]:
    return build_proxy_ws_headers(
        token,
        origin=origin,
        extra=extra,
        lease=lease,
    )


def build_headers(
    cookie_token: str,
    content_type: Optional[str] = None,
    origin: Optional[str] = None,
    referer: Optional[str] = None,
    *,
    lease: ProxyLease | None = None,
) -> Dict[str, str]:
    return build_http_headers(
        cookie_token,
        content_type=content_type,
        origin=origin,
        referer=referer,
        lease=lease,
    )


__all__ = ["build_headers", "build_sso_cookie", "build_ws_headers"]
