"""Admin config CRUD + verify + storage info."""

from __future__ import annotations

import os
import re

from fastapi import APIRouter, HTTPException
from fastapi.responses import JSONResponse

from app.platform.config.snapshot import config, get_config
from app.platform.logging.logger import logger, reload_logging

router = APIRouter()

# ---------------------------------------------------------------------------
# Proxy config sanitisation
# ---------------------------------------------------------------------------

_CFG_CHAR_REPLACEMENTS = str.maketrans({
    "\u2010": "-", "\u2011": "-", "\u2012": "-",
    "\u2013": "-", "\u2014": "-", "\u2212": "-",
    "\u2018": "'", "\u2019": "'", "\u201c": '"', "\u201d": '"',
    "\u00a0": " ", "\u2007": " ", "\u202f": " ",
    "\u200b": "", "\u200c": "", "\u200d": "", "\ufeff": "",
})


def _sanitize_text(value, *, remove_all_spaces: bool = False) -> str:
    text = "" if value is None else str(value)
    text = text.translate(_CFG_CHAR_REPLACEMENTS)
    if remove_all_spaces:
        text = re.sub(r"\s+", "", text)
    else:
        text = text.strip()
    return text.encode("latin-1", errors="ignore").decode("latin-1")


def _sanitize_proxy_config(data: dict) -> dict:
    if not isinstance(data, dict):
        return data
    payload = dict(data)
    proxy = payload.get("proxy")
    if not isinstance(proxy, dict):
        return payload

    sanitized = dict(proxy)
    changed = False
    for key, strip_spaces in [("user_agent", False), ("cf_cookies", False), ("cf_clearance", True)]:
        if key in sanitized:
            raw = sanitized[key]
            val = _sanitize_text(raw, remove_all_spaces=strip_spaces)
            if val != raw:
                sanitized[key] = val
                changed = True
    if changed:
        logger.warning("Sanitized proxy config fields before saving")
        payload["proxy"] = sanitized
    return payload


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

@router.get("/verify")
async def admin_verify():
    return {"status": "success"}


@router.get("/config")
async def get_config_endpoint():
    return JSONResponse(config.raw())


@router.post("/config")
async def update_config(data: dict):
    try:
        await config.update(_sanitize_proxy_config(data))
        reload_logging()
        return {"status": "success", "message": "配置已更新"}
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@router.get("/storage")
async def get_storage_mode():
    backend = get_config("account.storage", "local")
    return {"type": str(backend).strip().lower() or "local"}
