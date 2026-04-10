import ssl
import unittest
from unittest.mock import patch

from app.control.account.backends import sql


class SqlEngineSslTests(unittest.TestCase):
    def test_create_pgsql_engine_moves_sslmode_to_connect_args(self) -> None:
        with patch.object(sql, "create_async_engine", return_value=object()) as create_engine:
            sql.create_pgsql_engine(
                "postgres://user:pass@example.com:5432/defaultdb?sslmode=require&application_name=grok2api"
            )

        create_engine.assert_called_once()
        args, kwargs = create_engine.call_args
        self.assertEqual(
            args[0],
            "postgresql+asyncpg://user:pass@example.com:5432/defaultdb?application_name=grok2api",
        )
        self.assertEqual(kwargs["connect_args"], {"ssl": "require"})
        self.assertEqual(kwargs["pool_size"], 10)
        self.assertEqual(kwargs["max_overflow"], 20)
        self.assertTrue(kwargs["pool_pre_ping"])

    def test_create_mysql_engine_moves_ssl_mode_to_ssl_context(self) -> None:
        with patch.object(sql, "create_async_engine", return_value=object()) as create_engine:
            sql.create_mysql_engine(
                "mysql://user:pass@example.com:3306/defaultdb?ssl-mode=REQUIRED&charset=utf8mb4"
            )

        create_engine.assert_called_once()
        args, kwargs = create_engine.call_args
        self.assertEqual(
            args[0],
            "mysql+aiomysql://user:pass@example.com:3306/defaultdb?charset=utf8mb4",
        )
        self.assertIsInstance(kwargs["connect_args"]["ssl"], ssl.SSLContext)
        self.assertFalse(kwargs["connect_args"]["ssl"].check_hostname)
        self.assertEqual(kwargs["connect_args"]["ssl"].verify_mode, ssl.CERT_NONE)
        self.assertEqual(kwargs["pool_size"], 10)
        self.assertEqual(kwargs["max_overflow"], 20)
        self.assertTrue(kwargs["pool_pre_ping"])


if __name__ == "__main__":
    unittest.main()
