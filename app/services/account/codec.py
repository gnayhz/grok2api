"""
Common filtering, sorting and serialization helpers for account storage backends.
"""

from __future__ import annotations

import json
import math
from typing import Any, Iterable

from app.services.account.commands import ListAccountsQuery
from app.services.account.models import (
    AccountPage,
    AccountRecord,
    AccountSortField,
    AccountStatus,
    AccountSummary,
    SortDirection,
)


def compute_revision(previous: int = 0) -> int:
    from app.services.account.models import now_ms

    current = now_ms()
    return current if current > previous else previous + 1


def encode_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def decode_json(value: Any, default: Any) -> Any:
    if value is None or value == "":
        return default
    if isinstance(value, (dict, list)):
        return value
    try:
        return json.loads(str(value))
    except Exception:
        return default


def record_matches_query(record: AccountRecord, query: ListAccountsQuery) -> bool:
    if not query.include_deleted and record.deleted_at is not None:
        return False
    if query.pool_names and record.pool_name not in set(query.pool_names):
        return False
    if query.statuses and record.status not in set(query.statuses):
        return False
    if query.tags_all:
        tags = set(record.tags)
        if not set(query.tags_all).issubset(tags):
            return False
    if query.search:
        needle = query.search.lower()
        haystacks = [record.token.lower(), record.note.lower()]
        if needle not in " ".join(haystacks):
            return False
    return True


def sort_records(
    records: Iterable[AccountRecord],
    *,
    field: AccountSortField,
    direction: SortDirection,
) -> list[AccountRecord]:
    reverse = direction == SortDirection.DESC

    def sort_key(item: AccountRecord) -> tuple:
        if field == AccountSortField.UPDATED_AT:
            primary = item.updated_at
        elif field == AccountSortField.CREATED_AT:
            primary = item.created_at
        elif field == AccountSortField.LAST_USED_AT:
            primary = item.last_used_at or 0
        elif field == AccountSortField.QUOTA:
            primary = item.quota
        elif field == AccountSortField.CONSUMED:
            primary = item.consumed
        elif field == AccountSortField.USE_COUNT:
            primary = item.use_count
        else:
            primary = item.token
        return (primary, item.token)

    return sorted(records, key=sort_key, reverse=reverse)


def summarize_records(records: Iterable[AccountRecord]) -> AccountSummary:
    summary = AccountSummary()
    for record in records:
        summary.total += 1
        if record.deleted_at is not None:
            summary.deleted += 1
        if record.status == AccountStatus.ACTIVE:
            summary.active += 1
            summary.chat_quota += max(0, record.quota)
        elif record.status == AccountStatus.COOLING:
            summary.cooling += 1
        elif record.status == AccountStatus.EXPIRED:
            summary.expired += 1
        elif record.status == AccountStatus.DISABLED:
            summary.disabled += 1
        if "nsfw" in set(record.tags):
            summary.nsfw += 1
        else:
            summary.no_nsfw += 1
        summary.total_consumed += max(0, record.consumed)
        summary.total_calls += max(0, record.use_count)
    summary.image_quota = summary.chat_quota // 2
    return summary


def build_page(
    records: list[AccountRecord],
    *,
    query: ListAccountsQuery,
    revision: int,
) -> AccountPage:
    summary = summarize_records(records)
    filtered = [record for record in records if record_matches_query(record, query)]
    ordered = sort_records(
        filtered, field=query.sort_field, direction=query.sort_direction
    )
    total = len(ordered)
    total_pages = max(1, math.ceil(total / query.page_size)) if total else 1
    page = min(max(query.page, 1), total_pages)
    start = (page - 1) * query.page_size
    end = start + query.page_size
    return AccountPage(
        items=ordered[start:end],
        total=total,
        page=page,
        page_size=query.page_size,
        total_pages=total_pages,
        summary=summary,
        revision=revision,
    )

