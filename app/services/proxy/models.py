"""
Core models for the unified proxy / clearance domain.
"""

from __future__ import annotations

from enum import Enum
from typing import Optional

from pydantic import BaseModel, Field


class ProxyScope(str, Enum):
    APP = "app"
    ASSET = "asset"


class RequestKind(str, Enum):
    HTTP = "http"
    WS = "ws"


class EgressMode(str, Enum):
    DIRECT = "direct"
    SINGLE_PROXY = "single_proxy"
    PROXY_POOL = "proxy_pool"


class ClearanceMode(str, Enum):
    NONE = "none"
    MANUAL = "manual"
    MANAGED = "managed"


class EgressNodeState(str, Enum):
    ACTIVE = "active"
    COOLING = "cooling"
    DISABLED = "disabled"


class ClearanceBundleState(str, Enum):
    ACTIVE = "active"
    REFRESHING = "refreshing"
    COOLING = "cooling"
    EXPIRED = "expired"


class ProxyFeedbackKind(str, Enum):
    SUCCESS = "success"
    TRANSPORT_ERROR = "transport_error"
    RATE_LIMITED = "rate_limited"
    CHALLENGE = "challenge"
    FORBIDDEN = "forbidden"
    UNAUTHORIZED = "unauthorized"
    UPSTREAM_5XX = "upstream_5xx"


class EgressNode(BaseModel):
    node_id: str
    scope: ProxyScope
    mode: EgressMode
    proxy_url: str = ""
    state: EgressNodeState = EgressNodeState.ACTIVE
    health_score: float = Field(default=1.0, ge=0.0)


class ClearanceBundle(BaseModel):
    bundle_id: str
    mode: ClearanceMode
    affinity_key: str
    cf_cookies: str = ""
    cf_clearance: str = ""
    user_agent: str = ""
    browser: str = ""
    state: ClearanceBundleState = ClearanceBundleState.ACTIVE


class ProxyLease(BaseModel):
    lease_id: str
    scope: ProxyScope
    request_kind: RequestKind
    node_id: str
    bundle_id: Optional[str] = None
    proxy_url: str = ""
    cf_cookies: str = ""
    cf_clearance: str = ""
    user_agent: str = ""
    browser: str = ""
    selected_at: int


class ProxyFeedback(BaseModel):
    kind: ProxyFeedbackKind
    status_code: Optional[int] = None
    reason: str = ""
    at: int
    retry_after_ms: Optional[int] = None


__all__ = [
    "ClearanceBundle",
    "ClearanceBundleState",
    "ClearanceMode",
    "EgressMode",
    "EgressNode",
    "EgressNodeState",
    "ProxyFeedback",
    "ProxyFeedbackKind",
    "ProxyLease",
    "ProxyScope",
    "RequestKind",
]
