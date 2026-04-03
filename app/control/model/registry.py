"""Model registry — all supported model variants defined in one place."""

from __future__ import annotations

from .enums import Capability, ModeId, Tier
from .spec import ModelSpec

# ---------------------------------------------------------------------------
# Master model list.
# Add new models here; no other files need to change.
# ---------------------------------------------------------------------------

MODELS: tuple[ModelSpec, ...] = (
    # ── Basic · Chat ────────────────────────────────────────────────────────
    ModelSpec("grok-3",             ModeId.AUTO,   Tier.BASIC, Capability.CHAT,       True,  "Grok 3"),
    ModelSpec("grok-3-fast",        ModeId.FAST,   Tier.BASIC, Capability.CHAT,       True,  "Grok 3 Fast"),
    ModelSpec("grok-3-expert",      ModeId.EXPERT, Tier.BASIC, Capability.CHAT,       True,  "Grok 3 Expert"),
    ModelSpec("grok-4",             ModeId.AUTO,   Tier.BASIC, Capability.CHAT,       True,  "Grok 4"),
    ModelSpec("grok-4-fast",        ModeId.FAST,   Tier.BASIC, Capability.CHAT,       True,  "Grok 4 Fast"),
    ModelSpec("grok-4-expert",      ModeId.EXPERT, Tier.BASIC, Capability.CHAT,       True,  "Grok 4 Expert"),
    # ── Super · Chat ────────────────────────────────────────────────────────
    ModelSpec("grok-4-heavy",       ModeId.EXPERT, Tier.SUPER, Capability.CHAT,       True,  "Grok 4 Heavy"),
    # ── Basic · Image ───────────────────────────────────────────────────────
    ModelSpec("grok-image",         ModeId.FAST,   Tier.BASIC, Capability.IMAGE,      True,  "Grok Image"),
    ModelSpec("grok-image-fast",    ModeId.FAST,   Tier.BASIC, Capability.IMAGE,      True,  "Grok Image Fast"),
    ModelSpec("grok-image-edit",    ModeId.FAST,   Tier.BASIC, Capability.IMAGE_EDIT, True,  "Grok Image Edit"),
    # ── Basic · Video ───────────────────────────────────────────────────────
    ModelSpec("grok-video",         ModeId.FAST,   Tier.BASIC, Capability.VIDEO,      True,  "Grok Video"),
)

# ---------------------------------------------------------------------------
# Internal lookup structures — built once at import time.
# ---------------------------------------------------------------------------

_BY_NAME: dict[str, ModelSpec] = {m.model_name: m for m in MODELS}

_BY_CAP: dict[int, list[ModelSpec]] = {}
for _m in MODELS:
    _BY_CAP.setdefault(int(_m.capability), []).append(_m)


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

def get(model_name: str) -> ModelSpec | None:
    """Return the spec for *model_name*, or ``None`` if not registered."""
    return _BY_NAME.get(model_name)


def resolve(model_name: str) -> ModelSpec:
    """Return the spec for *model_name*; raise ``ValueError`` if unknown."""
    spec = _BY_NAME.get(model_name)
    if spec is None:
        raise ValueError(f"Unknown model: {model_name!r}")
    return spec


def list_enabled() -> list[ModelSpec]:
    """Return all enabled models in registration order."""
    return [m for m in MODELS if m.enabled]


def list_by_capability(cap: Capability) -> list[ModelSpec]:
    """Return enabled models that include *cap* in their capability mask."""
    return [m for m in MODELS if m.enabled and bool(m.capability & cap)]


__all__ = ["MODELS", "get", "resolve", "list_enabled", "list_by_capability"]
