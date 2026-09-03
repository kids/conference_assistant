"""LLM 客户端：兼容 OpenAI chat.completions 格式。

本项目默认对接 taiji 服务（asr.example.com/tencent/llm/taiji），
其 base_url 本身即为完整端点（无需拼接 /v1/chat/completions），
返回标准 OpenAI 格式，模型为思考模型（含 reasoning_content 字段）。
"""
from __future__ import annotations

import json
from collections.abc import Iterator

import httpx


class LlmError(Exception):
    pass


class OpenAICompatClient:
    def __init__(
        self,
        base_url: str,
        api_key: str,
        model: str,
        timeout: float = 6.0,
        chat_path: str = "",
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.timeout = timeout
        # chat_path 默认空 = base_url 即完整端点；标准 OpenAI 兼容服务可填 "/v1/chat/completions"
        self.chat_path = chat_path
        self.url = self.base_url + self.chat_path

    def _headers(self) -> dict:
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        return headers

    def _payload(self, messages: list[dict], *, max_tokens: int, temperature: float) -> dict:
        return {
            "model": self.model,
            "messages": messages,
            "max_tokens": max_tokens,
            "temperature": temperature,
        }

    def stream_chat(
        self,
        messages: list[dict],
        *,
        max_tokens: int = 300,
        temperature: float = 0.3,
    ) -> Iterator[str]:
        """流式生成，逐段 yield 正文 content（跳过思考内容 reasoning_content）。"""
        payload = self._payload(messages, max_tokens=max_tokens, temperature=temperature)
        payload["stream"] = True
        try:
            with httpx.Client(timeout=self.timeout) as client:
                with client.stream("POST", self.url, json=payload, headers=self._headers()) as resp:
                    if resp.status_code != 200:
                        raise LlmError(f"LLM {resp.status_code}: {resp.text[:200]}")
                    for line in resp.iter_lines():
                        if not line or not line.startswith("data:"):
                            continue
                        data = line[len("data:"):].strip()
                        if data == "[DONE]":
                            break
                        try:
                            obj = json.loads(data)
                        except Exception:
                            continue
                        choices = obj.get("choices") or []
                        if not choices:
                            continue
                        delta = choices[0].get("delta", {}) or {}
                        content = delta.get("content")
                        if content:
                            yield content
        except httpx.TimeoutException as e:
            raise LlmError("LLM 超时") from e
        except httpx.HTTPError as e:
            raise LlmError(f"LLM 连接失败: {e}") from e

    def chat(
        self,
        messages: list[dict],
        *,
        max_tokens: int = 2000,
        temperature: float = 0.3,
    ) -> str:
        """非流式生成，返回完整正文文本（用于热词/背景生成等一次性任务）。"""
        payload = self._payload(messages, max_tokens=max_tokens, temperature=temperature)
        payload["stream"] = False
        try:
            resp = httpx.post(self.url, json=payload, headers=self._headers(), timeout=self.timeout)
        except httpx.TimeoutException as e:
            raise LlmError("LLM 超时") from e
        except httpx.HTTPError as e:
            raise LlmError(f"LLM 连接失败: {e}") from e
        if resp.status_code != 200:
            raise LlmError(f"LLM {resp.status_code}: {resp.text[:200]}")
        data = resp.json()
        try:
            content = data["choices"][0]["message"].get("content")
        except (KeyError, IndexError, TypeError):
            content = None
        return (content or "").strip()
