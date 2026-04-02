"""
Redis account repository.

This backend is optimized for write throughput and runtime cache refresh.
"""

from __future__ import annotations

from typing import Optional, Sequence

from redis import asyncio as aioredis

from app.services.account.codec import (
    build_page,
    compute_revision,
    decode_json,
    encode_json,
)
from app.services.account.commands import AccountPatch, AccountUpsert, ListAccountsQuery
from app.services.account.models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountRecord,
    RuntimeSnapshot,
)
from app.services.account.repository import AccountRepository
from app.services.account.storage_layout import ACCOUNT_SCHEMA_VERSION


class RedisAccountRepository(AccountRepository):
    def __init__(self, url: str, namespace: str = "grok2api:account:v1"):
        self.redis = aioredis.from_url(
            url,
            decode_responses=True,
            health_check_interval=30,
        )
        self.namespace = namespace.rstrip(":")

    def _key(self, *parts: str) -> str:
        return ":".join([self.namespace, *parts])

    def _data_key(self, token: str) -> str:
        return self._key("data", token)

    def _pool_key(self, pool_name: str) -> str:
        return self._key("pool", pool_name)

    def _status_key(self, status: str) -> str:
        return self._key("status", status)

    def _tag_key(self, tag: str) -> str:
        return self._key("tag", tag)

    def _meta_key(self) -> str:
        return self._key("meta")

    async def initialize(self) -> None:
        await self.redis.hsetnx(self._meta_key(), "schema_version", ACCOUNT_SCHEMA_VERSION)
        await self.redis.hsetnx(self._meta_key(), "revision", "0")

    async def close(self) -> None:
        try:
            await self.redis.close()
        except Exception:
            pass

    def _record_to_hash(self, record: AccountRecord) -> dict[str, str]:
        return {
            "token": record.token,
            "pool_name": record.pool_name,
            "status": record.status.value,
            "quota": str(record.quota),
            "consumed": str(record.consumed),
            "created_at": str(record.created_at),
            "updated_at": str(record.updated_at),
            "last_used_at": "" if record.last_used_at is None else str(record.last_used_at),
            "use_count": str(record.use_count),
            "fail_count": str(record.fail_count),
            "last_fail_at": "" if record.last_fail_at is None else str(record.last_fail_at),
            "last_fail_reason": record.last_fail_reason or "",
            "last_sync_at": "" if record.last_sync_at is None else str(record.last_sync_at),
            "tags_json": encode_json(record.tags),
            "note": record.note or "",
            "last_asset_clear_at": "" if record.last_asset_clear_at is None else str(record.last_asset_clear_at),
            "metadata_json": encode_json(record.metadata),
            "deleted_at": "" if record.deleted_at is None else str(record.deleted_at),
        }

    def _hash_to_record(self, data: dict[str, str]) -> Optional[AccountRecord]:
        if not data or not data.get("token"):
            return None
        return AccountRecord(
            token=data["token"],
            pool_name=data["pool_name"],
            status=data["status"],
            quota=int(data.get("quota") or 0),
            consumed=int(data.get("consumed") or 0),
            created_at=int(data.get("created_at") or 0),
            updated_at=int(data.get("updated_at") or 0),
            last_used_at=int(data["last_used_at"]) if data.get("last_used_at") else None,
            use_count=int(data.get("use_count") or 0),
            fail_count=int(data.get("fail_count") or 0),
            last_fail_at=int(data["last_fail_at"]) if data.get("last_fail_at") else None,
            last_fail_reason=data.get("last_fail_reason") or None,
            last_sync_at=int(data["last_sync_at"]) if data.get("last_sync_at") else None,
            tags=decode_json(data.get("tags_json"), []),
            note=data.get("note") or "",
            last_asset_clear_at=int(data["last_asset_clear_at"]) if data.get("last_asset_clear_at") else None,
            metadata=decode_json(data.get("metadata_json"), {}),
            deleted_at=int(data["deleted_at"]) if data.get("deleted_at") else None,
        )

    async def get_revision(self) -> int:
        value = await self.redis.hget(self._meta_key(), "revision")
        return int(value or 0)

    async def get_metadata(self) -> dict[str, str]:
        return await self.redis.hgetall(self._meta_key())

    async def set_metadata(self, mapping: dict[str, str]) -> None:
        if mapping:
            await self.redis.hset(self._meta_key(), mapping=mapping)

    async def get_accounts(self, tokens: Sequence[str]) -> dict[str, AccountRecord]:
        token_list = [token.replace("sso=", "") for token in tokens]
        if not token_list:
            return {}
        async with self.redis.pipeline() as pipe:
            for token in token_list:
                pipe.hgetall(self._data_key(token))
            rows = await pipe.execute()
        records: dict[str, AccountRecord] = {}
        for row in rows:
            record = self._hash_to_record(row)
            if record:
                records[record.token] = record
        return records

    async def list_accounts(self, query: ListAccountsQuery):
        async with self.redis.pipeline() as pipe:
            pipe.smembers(self._key("tokens"))
            pipe.hget(self._meta_key(), "revision")
            result = await pipe.execute()
        tokens = sorted(result[0] or [])
        revision = int(result[1] or 0)
        records = list((await self.get_accounts(tokens)).values())
        return build_page(records, query=query, revision=revision)

    async def _remove_indexes(self, record: AccountRecord, pipe) -> None:
        pipe.srem(self._pool_key(record.pool_name), record.token)
        pipe.srem(self._status_key(record.status.value), record.token)
        for tag in record.tags:
            pipe.srem(self._tag_key(tag), record.token)

    async def _write_record(
        self,
        record: AccountRecord,
        previous: Optional[AccountRecord],
        pipe,
    ) -> None:
        if previous:
            await self._remove_indexes(previous, pipe)
        pipe.sadd(self._key("tokens"), record.token)
        pipe.hset(self._data_key(record.token), mapping=self._record_to_hash(record))
        pipe.zadd(self._key("updated"), {record.token: record.updated_at})
        pipe.sadd(self._pool_key(record.pool_name), record.token)
        pipe.sadd(self._status_key(record.status.value), record.token)
        for tag in record.tags:
            pipe.sadd(self._tag_key(tag), record.token)

    async def upsert_accounts(
        self, items: Sequence[AccountUpsert]
    ) -> AccountMutationResult:
        if not items:
            return AccountMutationResult(revision=await self.get_revision())
        existing = await self.get_accounts([item.token for item in items])
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        records = [
            item.to_record(
                current=existing.get(item.token.replace("sso=", "")),
                revision=revision,
            )
            for item in items
        ]
        async with self.redis.pipeline() as pipe:
            for record in records:
                await self._write_record(record, existing.get(record.token), pipe)
            pipe.hset(self._meta_key(), mapping={"revision": str(revision)})
            await pipe.execute()
        return AccountMutationResult(upserted=len(records), revision=revision)

    async def patch_accounts(
        self, patches: Sequence[AccountPatch]
    ) -> AccountMutationResult:
        if not patches:
            return AccountMutationResult(revision=await self.get_revision())
        current = await self.get_accounts([patch.token for patch in patches])
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        updated: list[AccountRecord] = []
        for patch in patches:
            token = patch.token.replace("sso=", "")
            record = current.get(token)
            if not record:
                continue
            updated.append(patch.apply(record, revision=revision))
        if not updated:
            return AccountMutationResult(revision=previous_revision)
        async with self.redis.pipeline() as pipe:
            for record in updated:
                await self._write_record(record, current.get(record.token), pipe)
            pipe.hset(self._meta_key(), mapping={"revision": str(revision)})
            await pipe.execute()
        return AccountMutationResult(patched=len(updated), revision=revision)

    async def delete_accounts(self, tokens: Sequence[str]) -> AccountMutationResult:
        token_list = [token.replace("sso=", "") for token in tokens]
        if not token_list:
            return AccountMutationResult(revision=await self.get_revision())
        current = await self.get_accounts(token_list)
        previous_revision = await self.get_revision()
        revision = compute_revision(previous_revision)
        deleted_count = 0
        async with self.redis.pipeline() as pipe:
            for token in token_list:
                record = current.get(token)
                if not record or record.deleted_at is not None:
                    continue
                deleted_count += 1
                deleted = record.model_copy(deep=True)
                deleted.deleted_at = revision
                deleted.updated_at = revision
                await self._write_record(deleted, record, pipe)
            pipe.hset(self._meta_key(), mapping={"revision": str(revision)})
            await pipe.execute()
        return AccountMutationResult(deleted=deleted_count, revision=revision)

    async def scan_changes(
        self, since_revision: int, *, limit: int = 5000
    ) -> AccountChangeSet:
        entries = await self.redis.zrangebyscore(
            self._key("updated"),
            min=since_revision + 1,
            max="+inf",
            start=0,
            num=limit + 1,
        )
        has_more = len(entries) > limit
        entries = entries[:limit]
        records = list((await self.get_accounts(entries)).values())
        revision = max([since_revision, *[record.updated_at for record in records]])
        return AccountChangeSet(
            revision=revision,
            items=[record for record in records if record.deleted_at is None],
            deleted_tokens=[record.token for record in records if record.deleted_at is not None],
            has_more=has_more,
        )

    async def runtime_snapshot(
        self, *, include_deleted: bool = False
    ) -> RuntimeSnapshot:
        tokens = sorted(await self.redis.smembers(self._key("tokens")) or [])
        records = list((await self.get_accounts(tokens)).values())
        if not include_deleted:
            records = [record for record in records if record.deleted_at is None]
        revision = max(
            (record.updated_at for record in records),
            default=await self.get_revision(),
        )
        return RuntimeSnapshot(revision=revision, items=records)
