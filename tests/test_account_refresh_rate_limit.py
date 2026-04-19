import asyncio
import tempfile
import unittest
from pathlib import Path

from app.control.account.backends.local import LocalAccountRepository
from app.control.account.commands import AccountUpsert
from app.control.account.enums import AccountStatus, QuotaSource
from app.control.account.refresh import AccountRefreshService
from app.platform.errors import UpstreamError


class AccountRefreshRateLimitTest(unittest.TestCase):
    def test_record_failure_persists_mode_quota_on_429(self) -> None:
        with tempfile.TemporaryDirectory() as tmpdir:
            db_path = Path(tmpdir) / "accounts.db"
            asyncio.run(self._run_case(db_path))

    async def _run_case(self, db_path: Path) -> None:
        repo = LocalAccountRepository(db_path)
        await repo.initialize()
        await repo.upsert_accounts([AccountUpsert(token="tok-basic")])

        service = AccountRefreshService(repo)
        await service.record_failure_async(
            "tok-basic",
            1,
            UpstreamError("rate limited", status=429),
        )

        record = (await repo.get_accounts(["tok-basic"]))[0]
        fast = record.quota_set().fast

        self.assertEqual(record.status, AccountStatus.ACTIVE)
        self.assertEqual(record.usage_fail_count, 1)
        self.assertEqual(record.last_fail_reason, "rate_limited")
        self.assertEqual(fast.remaining, 0)
        self.assertIsNotNone(fast.reset_at)
        self.assertEqual(fast.source, QuotaSource.ESTIMATED)


if __name__ == "__main__":
    unittest.main()
