"""
Account domain exceptions.
"""


class AccountError(Exception):
    """Base exception for the account domain."""


class AccountConflictError(AccountError):
    """Raised when a write conflicts with current persisted state."""


class AccountNotFoundError(AccountError):
    """Raised when an account cannot be found."""


class AccountBackendError(AccountError):
    """Raised when the underlying repository backend fails."""

