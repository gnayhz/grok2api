"""
Reverse interface: LiveKit token + WebSocket.
"""

from __future__ import annotations

from typing import Any, Dict
from urllib.parse import urlencode

import orjson
from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.account.token_service import TokenService
from app.services.config import get_config
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease, ProxyScope, RequestKind
from app.services.proxy.service import get_proxy_service
from app.services.proxy.session import build_http_proxies, build_session_kwargs
from app.services.reverse.utils.headers import build_headers, build_ws_headers
from app.services.reverse.utils.proxy import (
    classify_proxy_error,
    get_upstream_status,
    release_proxy_lease,
    report_proxy_lease,
)
from app.services.reverse.utils.retry import retry_on_status
from app.services.reverse.utils.websocket import WebSocketClient, WebSocketConnection

LIVEKIT_TOKEN_API = "https://grok.com/rest/livekit/tokens"
LIVEKIT_WS_URL = "wss://livekit.grok.com"


class LivekitTokenReverse:
    """/rest/livekit/tokens reverse interface."""

    @staticmethod
    async def request(
        session: AsyncSession,
        token: str,
        voice: str = "ara",
        personality: str = "assistant",
        speed: float = 1.0,
    ) -> Dict[str, Any]:
        """Fetch LiveKit token."""
        try:
            payload = {
                "sessionPayload": orjson.dumps(
                    {
                        "voice": voice,
                        "personality": personality,
                        "playback_speed": speed,
                        "enable_vision": False,
                        "turn_detection": {"type": "server_vad"},
                    }
                ).decode(),
                "requestAgentDispatch": False,
                "livekitUrl": LIVEKIT_WS_URL,
                "params": {"enable_markdown_transcript": "true"},
            }

            timeout = get_config("voice.timeout")
            default_browser = get_config("proxy.browser")
            proxy_service = get_proxy_service()
            active_lease: ProxyLease | None = None

            async def _do_request():
                nonlocal active_lease
                active_lease = await proxy_service.acquire(
                    scope=ProxyScope.APP,
                    request_kind=RequestKind.HTTP,
                )
                if active_lease is None:
                    raise UpstreamException(
                        message="LivekitTokenReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    content_type="application/json",
                    origin="https://grok.com",
                    referer="https://grok.com/",
                    lease=active_lease,
                )
                response = await session.post(
                    LIVEKIT_TOKEN_API,
                    headers=headers,
                    data=orjson.dumps(payload),
                    **build_session_kwargs(
                        lease=active_lease,
                        browser_override=default_browser,
                        kwargs={
                            "timeout": timeout,
                            "proxies": build_http_proxies(active_lease.proxy_url),
                        },
                    ),
                )

                if response.status_code != 200:
                    body = response.text[:200]
                    logger.error(
                        f"LivekitTokenReverse: Request failed, {response.status_code}, body={body}"
                    )
                    raise UpstreamException(
                        message=f"LivekitTokenReverse: Request failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "body": response.text,
                            "headers": dict(response.headers or {}),
                        },
                    )

                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                nonlocal active_lease
                await report_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="LivekitTokenReverse",
                    kind=classify_proxy_error(error, status_code),
                    status_code=status_code,
                    reason=f"retry_attempt_{attempt}",
                    retry_after_ms=int(delay * 1000),
                )
                active_lease = None

            response = await retry_on_status(_do_request, on_retry=_on_retry)
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="LivekitTokenReverse",
                kind=ProxyFeedbackKind.SUCCESS,
                status_code=200,
            )
            active_lease = None
            return response

        except Exception as e:
            if isinstance(e, UpstreamException):
                status = get_upstream_status(e)
                if status is not None:
                    await report_proxy_lease(
                        proxy_service,
                        active_lease,
                        label="LivekitTokenReverse",
                        kind=classify_proxy_error(e, status),
                        status_code=status,
                        reason="request_failed",
                    )
                    active_lease = None
                if status == 401:
                    try:
                        await TokenService.record_fail(token, status, "livekit_token_auth_failed")
                    except Exception:
                        pass
                raise

            logger.error(
                f"LivekitTokenReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="LivekitTokenReverse",
                kind=ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"LivekitTokenReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


class LivekitWebSocketReverse:
    """LiveKit WebSocket reverse interface."""

    def __init__(self) -> None:
        self._client = WebSocketClient()

    async def connect(self, token: str) -> WebSocketConnection:
        """Connect to the LiveKit WebSocket."""
        base = LIVEKIT_WS_URL.rstrip("/")
        if not base.endswith("/rtc"):
            base = f"{base}/rtc"

        params = {
            "access_token": token,
            "auto_subscribe": "1",
            "sdk": "js",
            "version": "2.11.4",
            "protocol": "15",
        }
        url = f"{base}?{urlencode(params)}"

        proxy_service = get_proxy_service()
        active_lease = await proxy_service.acquire(
            scope=ProxyScope.APP,
            request_kind=RequestKind.WS,
        )
        if active_lease is None:
            raise UpstreamException(
                "LivekitWebSocketReverse: Connect failed, proxy_lease_unavailable"
            )

        ws_headers = build_ws_headers(token=token, lease=active_lease)

        try:
            return await self._client.connect(
                url,
                headers=ws_headers,
                timeout=get_config("voice.timeout"),
                lease=active_lease,
                on_close=lambda: release_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="LivekitWebSocketReverse",
                ),
            )
        except Exception as e:
            status = get_upstream_status(e)
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="LivekitWebSocketReverse",
                kind=classify_proxy_error(e, status),
                status_code=status or 502,
                reason=type(e).__name__,
            )
            logger.error(f"LivekitWebSocketReverse: Connect failed, {e}")
            raise UpstreamException(
                f"LivekitWebSocketReverse: Connect failed, {str(e)}"
            )


__all__ = [
    "LivekitTokenReverse",
    "LivekitWebSocketReverse",
    "LIVEKIT_TOKEN_API",
    "LIVEKIT_WS_URL",
]
