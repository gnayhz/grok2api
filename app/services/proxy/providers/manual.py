"""
Manual clearance provider.
"""

from __future__ import annotations

from app.services.proxy.config import ProxyClearanceConfig
from app.services.proxy.models import ClearanceBundle, ClearanceMode


class ManualClearanceProvider:
    def build_bundle(
        self,
        *,
        config: ProxyClearanceConfig,
        affinity_key: str,
    ) -> ClearanceBundle | None:
        if config.mode != ClearanceMode.MANUAL:
            return None
        return ClearanceBundle(
            bundle_id=f"manual:{affinity_key}",
            mode=ClearanceMode.MANUAL,
            affinity_key=affinity_key,
            cf_cookies=config.cf_cookies,
            cf_clearance=config.cf_clearance,
            user_agent=config.user_agent,
            browser=config.browser,
        )


__all__ = ["ManualClearanceProvider"]
