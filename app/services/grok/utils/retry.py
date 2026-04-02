"""
Retry helpers for token switching.
"""

from typing import Optional, Set

from app.core.exceptions import UpstreamException
from app.services.grok.services.model import ModelService
from app.services.account.token_service import TokenService


async def pick_token(
    model_id: str,
    tried: Set[str],
    preferred: Optional[str] = None,
    prefer_tags: Optional[Set[str]] = None,
) -> Optional[str]:
    if preferred and preferred not in tried:
        return preferred

    pool_candidates = ModelService.pool_candidates_for_model(model_id)
    token = await TokenService.select_token(
        pool_candidates,
        exclude=tried,
        prefer_tags=prefer_tags,
    )

    if not token and not tried:
        await TokenService.refresh_tokens_on_demand()
        token = await TokenService.select_token(
            pool_candidates,
            exclude=tried,
            prefer_tags=prefer_tags,
        )

    return token


def rate_limited(error: Exception) -> bool:
    if not isinstance(error, UpstreamException):
        return False
    status = error.details.get("status") if error.details else None
    code = error.details.get("error_code") if error.details else None
    return status == 429 or code == "rate_limit_exceeded"


def transient_upstream(error: Exception) -> bool:
    """Whether error is likely transient and safe to retry with another token."""
    if not isinstance(error, UpstreamException):
        return False
    details = error.details or {}
    status = details.get("status")
    err = str(details.get("error") or error).lower()
    transient_status = {408, 500, 502, 503, 504}
    if status in transient_status:
        return True
    timeout_markers = (
        "timed out",
        "timeout",
        "connection reset",
        "temporarily unavailable",
        "http2",
    )
    return any(marker in err for marker in timeout_markers)


__all__ = ["pick_token", "rate_limited", "transient_upstream"]
