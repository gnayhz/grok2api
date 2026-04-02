__all__ = [
    "AccountMigrationReport",
    "load_legacy_tokens_from_source",
    "migrate_deprecated_config",
    "migrate_legacy_tokens_to_accounts",
]


def __getattr__(name: str):
    if name == "migrate_deprecated_config":
        from app.migrate.config import migrate_deprecated_config

        return migrate_deprecated_config
    if name in {
        "AccountMigrationReport",
        "load_legacy_tokens_from_source",
        "migrate_legacy_tokens_to_accounts",
    }:
        from app.migrate.account import (
            AccountMigrationReport,
            load_legacy_tokens_from_source,
            migrate_legacy_tokens_to_accounts,
        )

        return {
            "AccountMigrationReport": AccountMigrationReport,
            "load_legacy_tokens_from_source": load_legacy_tokens_from_source,
            "migrate_legacy_tokens_to_accounts": migrate_legacy_tokens_to_accounts,
        }[name]
    raise AttributeError(name)
