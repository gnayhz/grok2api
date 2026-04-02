"""
Minimal migration runner for the new account storage format.
"""

from __future__ import annotations

import argparse
import asyncio

from app.migrate.account import migrate_legacy_tokens_to_accounts
from app.services.account.factory import AccountRepositorySettings


async def _main() -> int:
    parser = argparse.ArgumentParser(description="Run Grok2API storage migrations.")
    parser.add_argument(
        "--force",
        action="store_true",
        help="Run migration even if the new account repository already has data.",
    )
    parser.add_argument(
        "--storage-type",
        default=None,
        help="Override target account storage type.",
    )
    parser.add_argument(
        "--storage-url",
        default=None,
        help="Override target account storage URL.",
    )
    args = parser.parse_args()

    settings = AccountRepositorySettings.from_env()
    if args.storage_type:
        settings.storage_type = args.storage_type
    if args.storage_url is not None:
        settings.storage_url = args.storage_url

    report = await migrate_legacy_tokens_to_accounts(
        settings=settings,
        force=args.force,
    )
    print(report.model_dump_json(indent=2))
    return 0


def main() -> int:
    return asyncio.run(_main())


if __name__ == "__main__":
    raise SystemExit(main())

