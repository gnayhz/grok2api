"""
Unified header builders for the proxy domain.
"""

from __future__ import annotations

import re
import uuid
import base64
import random
import string
from typing import Dict, Optional
from urllib.parse import urlparse

import orjson

from app.core.logger import logger
from app.services.config import get_config
from app.services.proxy.config import load_proxy_domain_config
from app.services.proxy.models import ProxyLease

_HEADER_CHAR_REPLACEMENTS = str.maketrans(
    {
        "\u2010": "-",
        "\u2011": "-",
        "\u2012": "-",
        "\u2013": "-",
        "\u2014": "-",
        "\u2212": "-",
        "\u2018": "'",
        "\u2019": "'",
        "\u201c": '"',
        "\u201d": '"',
        "\u00a0": " ",
        "\u2007": " ",
        "\u202f": " ",
        "\u200b": "",
        "\u200c": "",
        "\u200d": "",
        "\ufeff": "",
    }
)


def _statsig_rand(length: int, *, alphanumeric: bool = False) -> str:
    chars = (
        string.ascii_lowercase + string.digits
        if alphanumeric
        else string.ascii_lowercase
    )
    return "".join(random.choices(chars, k=length))


def _generate_statsig_id() -> str:
    dynamic = bool(get_config("app.dynamic_statsig"))
    if dynamic:
        if random.choice([True, False]):
            rand = _statsig_rand(5, alphanumeric=True)
            message = (
                "e:TypeError: Cannot read properties of null "
                f"(reading 'children['{rand}']')"
            )
        else:
            rand = _statsig_rand(10)
            message = (
                "e:TypeError: Cannot read properties of undefined "
                f"(reading '{rand}')"
            )
        return base64.b64encode(message.encode()).decode()
    return "ZTpUeXBlRXJyb3I6IENhbm5vdCByZWFkIHByb3BlcnRpZXMgb2YgdW5kZWZpbmVkIChyZWFkaW5nICdjaGlsZE5vZGVzJyk="


def sanitize_header_value(
    value: Optional[str],
    *,
    field_name: str,
    remove_all_spaces: bool = False,
) -> str:
    raw = "" if value is None else str(value)
    normalized = raw.translate(_HEADER_CHAR_REPLACEMENTS)
    if remove_all_spaces:
        normalized = re.sub(r"\s+", "", normalized)
    else:
        normalized = normalized.strip()

    normalized = normalized.encode("latin-1", errors="ignore").decode("latin-1")

    if normalized != raw:
        logger.warning(
            "Sanitized header field '{}' (len {} -> {})",
            field_name,
            len(raw),
            len(normalized),
        )
    return normalized


def _extract_major_version(browser: Optional[str], user_agent: Optional[str]) -> Optional[str]:
    if browser:
        match = re.search(r"(\d{2,3})", browser)
        if match:
            return match.group(1)
    if user_agent:
        for pattern in [r"Edg/(\d+)", r"Chrome/(\d+)", r"Chromium/(\d+)"]:
            match = re.search(pattern, user_agent)
            if match:
                return match.group(1)
    return None


def _detect_platform(user_agent: str) -> Optional[str]:
    ua = user_agent.lower()
    if "windows" in ua:
        return "Windows"
    if "mac os x" in ua or "macintosh" in ua:
        return "macOS"
    if "android" in ua:
        return "Android"
    if "iphone" in ua or "ipad" in ua:
        return "iOS"
    if "linux" in ua:
        return "Linux"
    return None


def _detect_arch(user_agent: str) -> Optional[str]:
    ua = user_agent.lower()
    if "aarch64" in ua or "arm" in ua:
        return "arm"
    if "x86_64" in ua or "x64" in ua or "win64" in ua or "intel" in ua:
        return "x86"
    return None


def _build_client_hints(browser: Optional[str], user_agent: Optional[str]) -> Dict[str, str]:
    browser = (browser or "").strip().lower()
    user_agent = user_agent or ""
    ua = user_agent.lower()

    is_edge = "edge" in browser or "edg" in ua
    is_brave = "brave" in browser
    is_chromium = any(key in browser for key in ["chrome", "chromium", "edge", "brave"]) or (
        "chrome" in ua or "chromium" in ua or "edg" in ua
    )
    is_firefox = "firefox" in ua or "firefox" in browser
    is_safari = ("safari" in ua and "chrome" not in ua and "chromium" not in ua and "edg" not in ua) or "safari" in browser

    if not is_chromium or is_firefox or is_safari:
        return {}

    version = _extract_major_version(browser, user_agent)
    if not version:
        return {}

    if is_edge:
        brand = "Microsoft Edge"
    elif "chromium" in browser:
        brand = "Chromium"
    elif is_brave:
        brand = "Brave"
    else:
        brand = "Google Chrome"

    sec_ch_ua = (
        f"\"{brand}\";v=\"{version}\", "
        f"\"Chromium\";v=\"{version}\", "
        "\"Not(A:Brand\";v=\"24\""
    )

    platform = _detect_platform(user_agent)
    arch = _detect_arch(user_agent)
    mobile = "?1" if ("mobile" in ua or platform in ("Android", "iOS")) else "?0"

    hints = {
        "Sec-Ch-Ua": sec_ch_ua,
        "Sec-Ch-Ua-Mobile": mobile,
    }
    if platform:
        hints["Sec-Ch-Ua-Platform"] = f"\"{platform}\""
    if arch:
        hints["Sec-Ch-Ua-Arch"] = arch
        hints["Sec-Ch-Ua-Bitness"] = "64"
    hints["Sec-Ch-Ua-Model"] = "" if mobile == "?0" else ""
    return hints


def _resolve_profile(lease: ProxyLease | None) -> tuple[str, str, str]:
    if lease is not None:
        return (
            lease.cf_cookies or "",
            lease.cf_clearance or "",
            lease.user_agent or "",
        )
    config = load_proxy_domain_config()
    return (
        config.clearance.cf_cookies,
        config.clearance.cf_clearance,
        config.clearance.user_agent,
    )


def _resolve_browser(lease: ProxyLease | None) -> str:
    if lease is not None and lease.browser:
        return lease.browser
    return load_proxy_domain_config().clearance.browser


def build_sso_cookie(
    sso_token: str,
    *,
    lease: ProxyLease | None = None,
    cf_cookies: str | None = None,
    cf_clearance: str | None = None,
) -> str:
    sso_token = sso_token[4:] if sso_token.startswith("sso=") else sso_token
    sso_token = sanitize_header_value(
        sso_token,
        field_name="sso_token",
        remove_all_spaces=True,
    )

    cookie = f"sso={sso_token}; sso-rw={sso_token}"

    profile_cookies, profile_clearance, _ = _resolve_profile(lease)
    effective_cf_cookies = sanitize_header_value(
        cf_cookies if cf_cookies is not None else profile_cookies,
        field_name="proxy.cf_cookies",
    )
    effective_cf_clearance = sanitize_header_value(
        cf_clearance if cf_clearance is not None else profile_clearance,
        field_name="proxy.cf_clearance",
        remove_all_spaces=True,
    )

    if effective_cf_clearance:
        if effective_cf_cookies:
            if re.search(r"(?:^|;\s*)cf_clearance=", effective_cf_cookies):
                effective_cf_cookies = re.sub(
                    r"(^|;\s*)cf_clearance=[^;]*",
                    r"\1cf_clearance=" + effective_cf_clearance,
                    effective_cf_cookies,
                    count=1,
                )
            else:
                effective_cf_cookies = effective_cf_cookies.rstrip("; ")
                effective_cf_cookies = (
                    f"{effective_cf_cookies}; cf_clearance={effective_cf_clearance}"
                )
        else:
            effective_cf_cookies = f"cf_clearance={effective_cf_clearance}"

    if effective_cf_cookies:
        if cookie and not cookie.endswith(";"):
            cookie += "; "
        cookie += effective_cf_cookies
    return cookie


def build_ws_headers(
    token: Optional[str] = None,
    *,
    origin: Optional[str] = None,
    extra: Optional[Dict[str, str]] = None,
    lease: ProxyLease | None = None,
) -> Dict[str, str]:
    _, _, raw_user_agent = _resolve_profile(lease)
    user_agent = sanitize_header_value(raw_user_agent, field_name="proxy.user_agent")
    browser = _resolve_browser(lease)
    safe_origin = sanitize_header_value(origin or "https://grok.com", field_name="origin")
    headers = {
        "Origin": safe_origin,
        "User-Agent": user_agent,
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Cache-Control": "no-cache",
        "Pragma": "no-cache",
    }

    client_hints = _build_client_hints(browser, user_agent)
    if client_hints:
        headers.update(client_hints)

    if token:
        headers["Cookie"] = build_sso_cookie(token, lease=lease)
    if extra:
        headers.update(extra)
    return headers


def build_http_headers(
    cookie_token: str,
    *,
    content_type: Optional[str] = None,
    origin: Optional[str] = None,
    referer: Optional[str] = None,
    lease: ProxyLease | None = None,
) -> Dict[str, str]:
    _, _, raw_user_agent = _resolve_profile(lease)
    user_agent = sanitize_header_value(raw_user_agent, field_name="proxy.user_agent")
    browser = _resolve_browser(lease)
    safe_origin = sanitize_header_value(origin or "https://grok.com", field_name="origin")
    safe_referer = sanitize_header_value(referer or "https://grok.com/", field_name="referer")

    headers = {
        "Accept-Encoding": "gzip, deflate, br, zstd",
        "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
        "Baggage": "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c",
        "Origin": safe_origin,
        "Priority": "u=1, i",
        "Referer": safe_referer,
        "Sec-Fetch-Mode": "cors",
        "User-Agent": user_agent,
    }

    client_hints = _build_client_hints(browser, user_agent)
    if client_hints:
        headers.update(client_hints)

    headers["Cookie"] = build_sso_cookie(cookie_token, lease=lease)

    if content_type == "application/json" or not content_type:
        headers["Content-Type"] = "application/json"
        headers["Accept"] = "*/*"
        headers["Sec-Fetch-Dest"] = "empty"
    elif content_type in ["image/jpeg", "image/png", "video/mp4", "video/webm"]:
        headers["Content-Type"] = content_type
        headers["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
        headers["Sec-Fetch-Dest"] = "document"
    else:
        headers["Content-Type"] = content_type
        headers["Accept"] = "*/*"
        headers["Sec-Fetch-Dest"] = "empty"

    origin_domain = urlparse(headers.get("Origin", "")).hostname
    referer_domain = urlparse(headers.get("Referer", "")).hostname
    if origin_domain and referer_domain and origin_domain == referer_domain:
        headers["Sec-Fetch-Site"] = "same-origin"
    else:
        headers["Sec-Fetch-Site"] = "same-site"

    headers["x-statsig-id"] = _generate_statsig_id()
    headers["x-xai-request-id"] = str(uuid.uuid4())

    safe_headers = dict(headers)
    if "Cookie" in safe_headers:
        safe_headers["Cookie"] = "<redacted>"
    logger.debug("Built headers: {}", orjson.dumps(safe_headers).decode())
    return headers


__all__ = [
    "build_http_headers",
    "build_sso_cookie",
    "build_ws_headers",
    "sanitize_header_value",
]
