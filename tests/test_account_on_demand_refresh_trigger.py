import unittest

from app.control.account.refresh import RefreshResult
from app.control.account.runtime import get_refresh_service, set_refresh_service
from app.platform.errors import UpstreamError
from app.products.openai.chat import _fail_sync


class _FakeRefreshService:
    def __init__(self) -> None:
        self.failure_calls: list[tuple[str, int, BaseException | None]] = []
        self.refresh_calls = 0

    async def record_failure_async(
        self,
        token: str,
        mode_id: int,
        exc: BaseException | None = None,
    ) -> None:
        self.failure_calls.append((token, mode_id, exc))

    async def refresh_on_demand(self) -> RefreshResult:
        self.refresh_calls += 1
        return RefreshResult(refreshed=3, failed=1, rate_limited=2)


class AccountOnDemandRefreshTriggerTest(unittest.IsolatedAsyncioTestCase):
    async def asyncTearDown(self) -> None:
        set_refresh_service(None)

    async def test_fail_sync_triggers_on_demand_refresh_for_429(self) -> None:
        svc = _FakeRefreshService()
        set_refresh_service(svc)

        await _fail_sync("tok-429", 1, UpstreamError("rate limited", status=429))

        self.assertEqual(len(svc.failure_calls), 1)
        self.assertEqual(svc.refresh_calls, 1)

    async def test_fail_sync_skips_on_demand_refresh_for_non_429(self) -> None:
        svc = _FakeRefreshService()
        set_refresh_service(svc)

        await _fail_sync("tok-500", 1, UpstreamError("upstream error", status=500))

        self.assertEqual(len(svc.failure_calls), 1)
        self.assertEqual(svc.refresh_calls, 0)


if __name__ == "__main__":
    unittest.main()
