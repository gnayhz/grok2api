"""ModelSpec — the single source of truth for model metadata."""

from __future__ import annotations

from dataclasses import dataclass

from .enums import Capability, ModeId, Tier


@dataclass(slots=True, frozen=True)
class ModelSpec:
    """Immutable descriptor for one model variant.

    ``model_name`` is the public-facing identifier used in API requests.
    ``mode_id``    is the upstream ``modeId`` value (auto / fast / expert).
    ``tier``       determines which account pool is used (basic / super).
    ``capability`` is a bitmask of supported operations.
    ``enabled``    gates whether the model appears in ``/v1/models``.
    ``public_name`` is the human-readable display name.
    """

    model_name:  str
    mode_id:     ModeId
    tier:        Tier
    capability:  Capability
    enabled:     bool
    public_name: str

    # --- convenience predicates ---

    def is_chat(self)       -> bool: return bool(self.capability & Capability.CHAT)
    def is_image(self)      -> bool: return bool(self.capability & Capability.IMAGE)
    def is_image_edit(self) -> bool: return bool(self.capability & Capability.IMAGE_EDIT)
    def is_video(self)      -> bool: return bool(self.capability & Capability.VIDEO)
    def is_voice(self)      -> bool: return bool(self.capability & Capability.VOICE)

    def pool_name(self) -> str:
        """Return the pool string expected by the account selector."""
        return "super" if self.tier == Tier.SUPER else "basic"

    def pool_id(self) -> int:
        """Return the integer PoolId for the dataplane account table."""
        return int(self.tier)


__all__ = ["ModelSpec"]
