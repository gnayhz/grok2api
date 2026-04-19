import unittest
from unittest.mock import patch

from app.platform.errors import UpstreamError
from app.dataplane.proxy.adapters.session import ResettableSession


class _RaisingSession:
    async def post(self, *args, **kwargs):
        raise RuntimeError("tls boom")

    async def close(self):
        return None


class TransportErrorWrappingTest(unittest.IsolatedAsyncioTestCase):
    async def test_resettable_session_wraps_transport_exception_as_upstream_502(self):
        with patch.object(ResettableSession, "_create", return_value=_RaisingSession()):
            session = ResettableSession()
            with self.assertRaises(UpstreamError) as ctx:
                await session.post("https://example.com")
            self.assertEqual(ctx.exception.status, 502)
            self.assertIn("tls boom", ctx.exception.message)
            await session.close()


if __name__ == "__main__":
    unittest.main()
