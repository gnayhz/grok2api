import unittest
from unittest.mock import patch

import orjson
from fastapi.responses import JSONResponse, StreamingResponse

from app.products.anthropic.router import MessagesRequest, messages_endpoint
from app.products.openai.router import chat_completions_endpoint, responses_endpoint
from app.products.openai.schemas import ChatCompletionRequest, MessageItem, ResponsesCreateRequest


class _FakeSpec:
    enabled = True

    def is_image_edit(self) -> bool:
        return False

    def is_image(self) -> bool:
        return False

    def is_video(self) -> bool:
        return False


class _FakeConfig:
    def get_bool(self, key: str, default: bool) -> bool:
        return default


async def _fake_stream():
    yield "data: hello\n\n"


class PublicApiStreamDefaultTest(unittest.IsolatedAsyncioTestCase):
    async def test_chat_endpoint_defaults_omitted_stream_to_json(self) -> None:
        calls: list[dict] = []

        async def _fake_chat_completions(**kwargs):
            calls.append(kwargs)
            return {"ok": True}

        req = ChatCompletionRequest(
            model="grok-4.20-auto",
            messages=[MessageItem(role="user", content="hi")],
        )

        with patch("app.products.openai.router.model_registry.get", return_value=_FakeSpec()):
            with patch("app.products.openai.router.chat_completions", new=_fake_chat_completions):
                response = await chat_completions_endpoint(req)

        self.assertIsInstance(response, JSONResponse)
        self.assertEqual(orjson.loads(response.body), {"ok": True})
        self.assertEqual(calls[0]["stream"], False)

    async def test_chat_endpoint_omitted_stream_propagates_json_errors(self) -> None:
        async def _boom(**kwargs):
            raise RuntimeError("boom")

        req = ChatCompletionRequest(
            model="grok-4.20-auto",
            messages=[MessageItem(role="user", content="hi")],
        )

        with patch("app.products.openai.router.model_registry.get", return_value=_FakeSpec()):
            with patch("app.products.openai.router.chat_completions", new=_boom):
                with self.assertRaisesRegex(RuntimeError, "boom"):
                    await chat_completions_endpoint(req)

    async def test_chat_endpoint_keeps_explicit_streaming(self) -> None:
        async def _fake_chat_completions(**kwargs):
            return _fake_stream()

        req = ChatCompletionRequest(
            model="grok-4.20-auto",
            messages=[MessageItem(role="user", content="hi")],
            stream=True,
        )

        with patch("app.products.openai.router.model_registry.get", return_value=_FakeSpec()):
            with patch("app.products.openai.router.chat_completions", new=_fake_chat_completions):
                response = await chat_completions_endpoint(req)

        self.assertIsInstance(response, StreamingResponse)
        self.assertEqual(response.media_type, "text/event-stream")

    async def test_responses_endpoint_defaults_omitted_stream_to_json(self) -> None:
        calls: list[dict] = []

        async def _fake_responses_create(**kwargs):
            calls.append(kwargs)
            return {"ok": True}

        req = ResponsesCreateRequest(
            model="grok-4.20-auto",
            input="hi",
        )

        with patch("app.products.openai.router.model_registry.get", return_value=_FakeSpec()):
            with patch("app.platform.config.snapshot.get_config", return_value=_FakeConfig()):
                with patch("app.products.openai.responses.create", new=_fake_responses_create):
                    response = await responses_endpoint(req)

        self.assertIsInstance(response, JSONResponse)
        self.assertEqual(orjson.loads(response.body), {"ok": True})
        self.assertEqual(calls[0]["stream"], False)

    async def test_messages_endpoint_defaults_omitted_stream_to_json(self) -> None:
        calls: list[dict] = []

        async def _fake_messages_create(**kwargs):
            calls.append(kwargs)
            return {"ok": True}

        req = MessagesRequest(
            model="grok-4.20-auto",
            messages=[{"role": "user", "content": "hi"}],
        )

        with patch("app.products.anthropic.router.model_registry.get", return_value=_FakeSpec()):
            with patch("app.platform.config.snapshot.get_config", return_value=_FakeConfig()):
                with patch("app.products.anthropic.messages.create", new=_fake_messages_create):
                    response = await messages_endpoint(req)

        self.assertIsInstance(response, JSONResponse)
        self.assertEqual(orjson.loads(response.body), {"ok": True})
        self.assertEqual(calls[0]["stream"], False)


if __name__ == "__main__":
    unittest.main()
