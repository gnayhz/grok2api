"""
Reverse interface: NSFW feature controls (gRPC-Web).
"""

from __future__ import annotations

from curl_cffi.requests import AsyncSession

from app.core.exceptions import UpstreamException
from app.core.logger import logger
from app.services.config import get_config
from app.services.proxy.models import ProxyFeedbackKind, ProxyLease, ProxyScope, RequestKind
from app.services.proxy.service import get_proxy_service
from app.services.proxy.session import build_http_proxies, build_session_kwargs
from app.services.reverse.utils.grpc import GrpcClient, GrpcStatus
from app.services.reverse.utils.headers import build_headers
from app.services.reverse.utils.proxy import (
    classify_proxy_error,
    get_upstream_status,
    report_proxy_lease,
)
from app.services.reverse.utils.retry import retry_on_status

NSFW_MGMT_API = "https://grok.com/auth_mgmt.AuthManagement/UpdateUserFeatureControls"


class NsfwMgmtReverse:
    """/auth_mgmt.AuthManagement/UpdateUserFeatureControls reverse interface."""

    @staticmethod
    async def request(session: AsyncSession, token: str) -> GrpcStatus:
        """Enable NSFW feature control via gRPC-Web."""
        try:
            name = "always_show_nsfw_content".encode("utf-8")
            inner = b"\x0a" + bytes([len(name)]) + name
            protobuf = b"\x0a\x02\x10\x01\x12" + bytes([len(inner)]) + inner
            payload = GrpcClient.encode_payload(protobuf)
            timeout = get_config("nsfw.timeout")
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
                        message="NsfwMgmtReverse: unable to acquire proxy lease",
                        details={"status": 502, "error": "proxy_lease_unavailable"},
                    )

                headers = build_headers(
                    cookie_token=token,
                    origin="https://grok.com",
                    referer="https://grok.com/?_s=data",
                    lease=active_lease,
                )
                headers["Content-Type"] = "application/grpc-web+proto"
                headers["Accept"] = "*/*"
                headers["Sec-Fetch-Dest"] = "empty"
                headers["x-grpc-web"] = "1"
                headers["x-user-agent"] = "connect-es/2.1.1"
                headers["Cache-Control"] = "no-cache"
                headers["Pragma"] = "no-cache"

                response = await session.post(
                    NSFW_MGMT_API,
                    headers=headers,
                    data=payload,
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
                    logger.error(
                        f"NsfwMgmtReverse: Request failed, {response.status_code}",
                        extra={"error_type": "UpstreamException"},
                    )
                    raise UpstreamException(
                        message=f"NsfwMgmtReverse: Request failed, {response.status_code}",
                        details={
                            "status": response.status_code,
                            "headers": dict(response.headers or {}),
                        },
                    )

                logger.debug(f"NsfwMgmtReverse: Request successful, {response.status_code}")
                return response

            async def _on_retry(attempt: int, status_code: int, error: Exception, delay: float):
                nonlocal active_lease
                await report_proxy_lease(
                    proxy_service,
                    active_lease,
                    label="NsfwMgmtReverse",
                    kind=classify_proxy_error(error, status_code),
                    status_code=status_code,
                    reason=f"retry_attempt_{attempt}",
                    retry_after_ms=int(delay * 1000),
                )
                active_lease = None

            response = await retry_on_status(_do_request, on_retry=_on_retry)

            _, trailers = GrpcClient.parse_response(
                response.content,
                content_type=response.headers.get("content-type"),
                headers=response.headers,
            )
            grpc_status = GrpcClient.get_status(trailers)
            if grpc_status.code not in (-1, 0):
                raise UpstreamException(
                    message=f"NsfwMgmtReverse: gRPC failed, {grpc_status.code}",
                    details={
                        "status": grpc_status.http_equiv,
                        "grpc_status": grpc_status.code,
                        "grpc_message": grpc_status.message,
                    },
                )

            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="NsfwMgmtReverse",
                kind=ProxyFeedbackKind.SUCCESS,
                status_code=200,
            )
            active_lease = None
            return grpc_status

        except Exception as e:
            if isinstance(e, UpstreamException):
                status = get_upstream_status(e)
                if status is not None:
                    await report_proxy_lease(
                        proxy_service,
                        active_lease,
                        label="NsfwMgmtReverse",
                        kind=classify_proxy_error(e, status),
                        status_code=status,
                        reason="request_failed",
                    )
                    active_lease = None
                raise

            logger.error(
                f"NsfwMgmtReverse: Request failed, {str(e)}",
                extra={"error_type": type(e).__name__},
            )
            await report_proxy_lease(
                proxy_service,
                active_lease,
                label="NsfwMgmtReverse",
                kind=ProxyFeedbackKind.TRANSPORT_ERROR,
                status_code=502,
                reason=type(e).__name__,
            )
            raise UpstreamException(
                message=f"NsfwMgmtReverse: Request failed, {str(e)}",
                details={"status": 502, "error": str(e)},
            )


__all__ = ["NsfwMgmtReverse"]
