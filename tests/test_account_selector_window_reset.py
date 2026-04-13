import unittest

from app.dataplane.account.selector import select
from app.dataplane.account.table import make_empty_table
from app.dataplane.shared.enums import PoolId, StatusId


class AccountSelectorWindowResetTest(unittest.TestCase):
    def test_expired_basic_window_resets_to_stored_total(self) -> None:
        table = make_empty_table()
        idx = table._append_slot(
            token="tok-basic",
            pool_id=int(PoolId.BASIC),
            status_id=int(StatusId.ACTIVE),
            quota_auto=0,
            quota_fast=0,
            quota_expert=0,
            quota_heavy=-1,
            total_auto=2,
            total_fast=6,
            total_expert=2,
            total_heavy=0,
            window_auto=7200,
            window_fast=7200,
            window_expert=7200,
            window_heavy=0,
            reset_auto=100,
            reset_fast=100,
            reset_expert=100,
            reset_heavy=0,
            health=1.0,
            last_use_s=0,
            last_fail_s=0,
            fail_count=0,
            tags=[],
        )

        selected = select(table, int(PoolId.BASIC), 0, now_s=200)

        self.assertEqual(selected, idx)
        self.assertEqual(int(table.quota_auto_by_idx[idx]), 2)
        self.assertEqual(int(table.reset_auto_at_by_idx[idx]), 200 + 7200)


if __name__ == "__main__":
    unittest.main()
