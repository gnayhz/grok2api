"""
FlareSolverr-backed managed clearance provider.
"""

from __future__ import annotations

import asyncio
import json
from urllib import request as urllib_request
from urllib.error import HTTPError, URLError

from app.core.logger import logger
from app.services.proxy.config import ProxyClearanceConfig
from app.services.proxy.models import ClearanceBundle, ClearanceMode


def _extract_all_cookies(cookies: list[dict]) -> str:
    return "; ".join([f"{item.get('name')}={item.get('value')}" for item in cookies])


def _extract_cookie_value(cookies: list[dict], name: str) -> str:
    for cookie in cookies:
        if cookie.get("name") == name:
            return cookie.get("value") or ""
    return ""


def _extract_browser_profile(user_agent: str) -> str:
    import re

    match = re.search(r"Chrome/(\d+)", user_agent)
    if match:
        return f"chrome{match.group(1)}"
    return "chrome120"


class FlareSolverrClearanceProvider:
    async def refresh_bundle(
        self,
        *,
        config: ProxyClearanceConfig,
        affinity_key: str,
        proxy_url: str,
    ) -> ClearanceBundle | None:
        if config.mode != ClearanceMode.MANAGED or not config.flaresolverr_url:
            return None

        result = await self.solve_cf_challenge(
            config=config,
            proxy_url=proxy_url,
        )
        if not result:
            logger.warning(
                "Proxy managed clearance refresh failed: affinity={} proxy={}",
                affinity_key,
                proxy_url or "<direct>",
            )
            return None

        return ClearanceBundle(
            bundle_id=f"managed:{affinity_key}",
            mode=ClearanceMode.MANAGED,
            affinity_key=affinity_key,
            cf_cookies=result.get("cookies", "") or "",
            cf_clearance=result.get("cf_clearance", "") or "",
            user_agent=result.get("user_agent", "") or "",
            browser=result.get("browser", "") or "",
        )

    async def solve_cf_challenge(
        self,
        *,
        config: ProxyClearanceConfig,
        proxy_url: str,
    ) -> dict[str, str] | None:
        payload = {
            "cmd": "request.get",
            "url": "https://grok.com",
            "maxTimeout": config.timeout_sec * 1000,
        }
        if proxy_url:
            payload["proxy"] = {"url": proxy_url}

        body = json.dumps(payload).encode("utf-8")
        headers = {"Content-Type": "application/json"}
        request = urllib_request.Request(
            f"{config.flaresolverr_url.rstrip('/')}/v1",
            data=body,
            method="POST",
            headers=headers,
        )

        try:
            def _post():
                with urllib_request.urlopen(request, timeout=config.timeout_sec + 30) as response:
                    return json.loads(response.read().decode("utf-8"))

            result = await asyncio.to_thread(_post)
            if result.get("status") != "ok":
                logger.warning(
                    "FlareSolverr returned non-ok status: {} {}",
                    result.get("status"),
                    result.get("message", ""),
                )
                return None

            solution = result.get("solution", {})
            cookies = solution.get("cookies", [])
            if not cookies:
                logger.warning("FlareSolverr returned no cookies")
                return None

            user_agent = solution.get("userAgent", "") or ""
            return {
                "cookies": _extract_all_cookies(cookies),
                "cf_clearance": _extract_cookie_value(cookies, "cf_clearance"),
                "user_agent": user_agent,
                "browser": _extract_browser_profile(user_agent),
            }
        except HTTPError as error:
            body_text = error.read().decode("utf-8", "replace")[:300]
            logger.warning("FlareSolverr HTTP error: {} {}", error.code, body_text)
            return None
        except URLError as error:
            logger.warning("FlareSolverr connect error: {}", error.reason)
            return None
        except Exception as error:
            logger.warning("FlareSolverr request failed: {}", error)
            return None


__all__ = ["FlareSolverrClearanceProvider"]
