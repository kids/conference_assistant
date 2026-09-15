"""智能体编排：组装 prompt → 流式生成 → 后置校验 → 发布事件。"""
from __future__ import annotations

import hashlib
import json
import threading
import time

from app.agent.guard import validate
from app.agent.tasks import TASKS, SYSTEM_PROMPT, estimate_secs
from app.config import Settings
from app.context.manager import ContextManager
from app.events import bus
from app.providers.llm_openai import LlmError, OpenAICompatClient
from app.state import display


PLAIN_LANGUAGE = "白话"


def _target_discipline_block(d: str) -> str:
    """生成「翻译目标学科」约束块。

    与 session.discipline（报告人学科）不同 —— 这里指的是「把内容翻译成给谁看」。
    """
    d = (d or "").strip() or PLAIN_LANGUAGE
    if d == PLAIN_LANGUAGE:
        return "【翻译目标学科】白话 —— 用非专业听众也能听懂的日常语言表达，避免堆砌专业术语。"
    return f"【翻译目标学科】{d} —— 请把输出调整到该学科的研究者能直接理解、能据此提问的表达层次。"


def _mock_output(task_key: str, target: str | None) -> str:
    """LLM 未配置时的占位输出，便于本地联调前端链路。"""
    if task_key == "TERM":
        return f"这个术语“{target or '该概念'}”可以理解为：用其他学科更容易把握的方式来看待它，抓住它的核心作用即可。它在本报告中重要，是因为它贯穿了作者要解决的关键问题。这个解释请报告人确认。"
    if task_key == "Q_TRANSLATE":
        return "刚才的问题更一般地说是在问：这个机制背后的普遍规律是什么，它可能与更广泛的研究领域也有关。这个转译请提问者和报告人确认。"
    if task_key == "A_TRANSLATE":
        return "报告人刚才的回答，通俗地说，是在解释为什么会得到这样的结果，以及这个结果意味着什么。这个转述请报告人确认。"
    if task_key == "BRIDGE":
        return "一个可以追问的问题是：这个方法背后的思路，能否用于解决另一个领域的相似困难？这个问题可能连接当前领域和更广泛的交叉领域。这个连接是否成立，请报告人判断。"
    return "刚才的报告围绕一个核心问题展开，报告人介绍了背景、方法和主要发现，并说明了其学术意义。以上总结请报告人确认。"


class AgentRunner:
    def __init__(self, settings: Settings, context: ContextManager) -> None:
        self.settings = settings
        self.context = context
        self.llm: OpenAICompatClient | None = None
        if settings.llm_base and settings.llm_model:
            self.llm = OpenAICompatClient(
                settings.llm_base, settings.llm_key, settings.llm_model,
                settings.llm_timeout, settings.llm_chat_path,
            )

    def generate(self, sid: str, task_key: str, target: str | None = None,
                 focus_segs: list[dict] | None = None,
                 target_discipline: str = PLAIN_LANGUAGE) -> str:
        """同步生成，返回 invocation_id；内部发布 AI_STATE/AI_DELTA/AI_READY 事件。

        target_discipline：把输出调整到该学科的表达层次（空值按「白话」处理）。
        """
        task = TASKS[task_key]
        ctx = self.context.build(sid, task_key, target, focus_segs)
        messages = self._build_messages(task, target, ctx, target_discipline)
        prompt_hash = hashlib.md5(json.dumps(messages, ensure_ascii=False).encode()).hexdigest()[:8]

        iid = f"iv{int(time.time() * 1000) % 1000000:06d}"
        display.begin_generate(iid, task_key)
        bus.publish({"type": "AI_STATE", "state": "GENERATING", "task": task_key, "invocation_id": iid})

        t0 = time.time()
        parts: list[str] = []
        try:
            if self.llm is not None:
                for delta in self.llm.stream_chat(
                    messages,
                    max_tokens=self.settings.llm_max_tokens,
                    temperature=self.settings.llm_temperature,
                ):
                    if display.killed:
                        break
                    parts.append(delta)
                    bus.publish({"type": "AI_DELTA", "invocation_id": iid, "delta": delta})
            else:
                # 未配置 LLM：模拟流式输出占位文本
                full = _mock_output(task_key, target)
                for ch in full:
                    if display.killed:
                        break
                    parts.append(ch)
                    time.sleep(0.015)
                    bus.publish({"type": "AI_DELTA", "invocation_id": iid, "delta": ch})
        except LlmError as e:
            bus.publish({"type": "AI_STATE", "state": "IDLE", "invocation_id": iid, "error": str(e)})
            display.reset()
            raise
        except Exception as e:  # noqa: BLE001
            bus.publish({"type": "AI_STATE", "state": "IDLE", "invocation_id": iid, "error": f"生成异常:{e}"})
            display.reset()
            raise

        if display.killed:
            bus.publish({"type": "AI_KILLED", "invocation_id": iid})
            display.reset()
            return iid

        raw = "".join(parts).strip()
        result = validate(raw, task.min_chars, task.max_chars)
        gen_ms = (time.time() - t0) * 1000

        if not result["ok"]:
            failed = [f"{k}:{v}" for k, v in result["checks"].items() if v != "ok"]
            hint = "校验未通过: " + ", ".join(failed)
            if not raw:
                # 正文为空：思考模型的 reasoning 可能吃光了 token 预算
                hint += "（模型未返回正文，可提高 LLM_MAX_TOKENS 后重试）"
            bus.publish({
                "type": "AI_STATE", "state": "IDLE", "invocation_id": iid,
                "error": hint,
                # 保留原文供操作员人工判断，避免白屏无从下手
                "raw_text": raw,
            })
            display.reset()
            return iid

        display.ready()
        bus.publish({
            "type": "AI_READY",
            "invocation_id": iid,
            "text": result["text"],
            "chars": len(result["text"]),
            "est_sec": round(estimate_secs(result["text"]), 1),
            "checks": result["checks"],
        })
        # 落库
        self.context.store.add_invocation(
            sid, task_key, target or "", prompt_hash, result["text"], result["checks"], gen_ms, iid=iid
        )
        return iid

    def _build_messages(self, task, target: str | None, ctx: dict,
                        target_discipline: str = PLAIN_LANGUAGE) -> list[dict]:
        user_parts = [
            "【会议材料（摘要/PPT/术语表）】\n" + (ctx["static"] or "（无）"),
            "【最近 30 分钟转写】\n" + (ctx["recent"] or "（无）"),
        ]
        if ctx["focus"]:
            user_parts.append("【当前焦点（主持人框选/最近 60 秒）】\n" + ctx["focus"])
        if ctx["history"]:
            user_parts.append("【本场已展示过的 AI 输出（避免重复）】\n" + ctx["history"])
        if target:
            user_parts.append(f"【{task.target_hint or '目标'}】\n{target}")
        user_parts.append(_target_discipline_block(target_discipline))
        user_parts.append("【任务】\n" + task.instruction)
        user = "\n\n".join(user_parts)
        return [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": user},
        ]
