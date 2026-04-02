"""
Canonical storage layout constants for the account domain.
"""

from pathlib import Path


ACCOUNT_SCHEMA_VERSION = "1"

LOCAL_ACCOUNT_SUBDIR = Path("account") / "v1"
LOCAL_ACCOUNT_DB_NAME = "accounts.db"

REDIS_ACCOUNT_NAMESPACE = "grok2api:account:v1"

SQL_ACCOUNT_RECORDS_TABLE = "account_records_v1"
SQL_ACCOUNT_META_TABLE = "account_meta_v1"

