"""Voice token endpoint — LiveKit token acquisition."""

from __future__ import annotations

from fastapi import APIRouter
from pydantic import BaseModel

from app.platform.errors import AppError, RateLimitError, UpstreamError
from app.platform.logging.logger import logger
from app.platform.runtime.clock import now_s

router = APIRouter()


class VoiceTokenResponse(BaseModel):
    token: str
    url: str
    participant_name: str = ""
    room_name: str = ""


@router.get("/voice/token", response_model=VoiceTokenResponse)
async def voice_token(
    voice: str = "ara",
    personality: str = "assistant",
    speed: float = 1.0,
):
    """Acquire a LiveKit voice session token."""
    from app.dataplane.account import _directory as _acct_dir
    if _acct_dir is None:
        raise RateLimitError("Account directory not initialised")

    # Voice uses super pools (ssoBasic/ssoSuper) → try super first, then basic.
    from app.dataplane.shared.enums import PoolId
    from app.control.model.enums import ModeId

    ts = now_s()
    acct = await _acct_dir.reserve(pool_id=int(PoolId.SUPER), mode_id=int(ModeId.AUTO), now_s_override=ts)
    if acct is None:
        acct = await _acct_dir.reserve(pool_id=int(PoolId.BASIC), mode_id=int(ModeId.AUTO), now_s_override=ts)
    if acct is None:
        raise RateLimitError("No available tokens for voice mode")

    token = acct.token
    try:
        from app.dataplane.reverse.transport.livekit import fetch_livekit_token
        data = await fetch_livekit_token(token, voice=voice, personality=personality, speed=speed)
        lk_token = data.get("token")
        if not lk_token:
            raise UpstreamError("Upstream returned no voice token")
        return VoiceTokenResponse(
            token=lk_token,
            url="wss://livekit.grok.com",
        )
    except AppError:
        raise
    except Exception as e:
        raise UpstreamError(f"Voice token error: {e}")
    finally:
        await _acct_dir.release(acct)
