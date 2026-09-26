#!/usr/bin/env python3
"""Run one local LM Studio HTML task and record usage/latency.

The script intentionally uses only the Python standard library so it can be
used before the full Go agent is configured. It speaks the OpenAI-compatible
``/v1/chat/completions`` endpoint exposed by LM Studio.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path


SYSTEM_PROMPT = """You are a careful frontend coding agent.
Return only one complete standalone HTML document, with no Markdown fences and
no explanation outside the document. Use only inline HTML, CSS, and JavaScript;
do not load external libraries, fonts, images, or network resources.
"""

TASK_PROMPT = """Create a simple but playable Tetris game as a single HTML file.

Requirements:
- Use a canvas or CSS grid and make the board visually clear.
- Support left/right movement, soft drop, hard drop, rotation, pause, and restart.
- Show score, cleared lines, level, and the next piece.
- Use keyboard controls and include a short on-page controls hint.
- Keep the implementation self-contained and reasonably easy to read.
- Return only the complete HTML document.
"""


def http_json(url: str, payload: dict | None, token: str | None) -> dict:
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(url, data=data, method="GET" if data is None else "POST")
    request.add_header("Accept", "application/json")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        detail = error.read().decode("utf-8", errors="replace")
        if error.code == 401:
            raise RuntimeError(
                "LM Studio 要求 API token；请设置 LM_API_TOKEN，或在 Developer > "
                "Server Settings 关闭 Require Authentication。"
            ) from error
        raise RuntimeError(f"LM Studio HTTP {error.code}: {detail[:1000]}") from error
    except urllib.error.URLError as error:
        raise RuntimeError(f"无法连接 LM Studio: {error.reason}") from error


def model_id(base_url: str, requested: str | None, token: str | None) -> str:
    if requested:
        return requested
    result = http_json(f"{base_url}/models", None, token)
    models = result.get("data", [])
    if not models or not models[0].get("id"):
        raise RuntimeError("LM Studio /v1/models 没有返回可用模型，请先加载模型")
    return str(models[0]["id"])


def clean_html(content: str) -> str:
    text = content.strip()
    fenced = re.match(r"^```(?:html)?\s*(.*?)\s*```$", text, flags=re.IGNORECASE | re.DOTALL)
    if fenced:
        text = fenced.group(1).strip()
    start = re.search(r"<!doctype html|<html(?:\s|>)", text, flags=re.IGNORECASE)
    if start:
        text = text[start.start() :]
    return text + ("\n" if text and not text.endswith("\n") else "")


def main() -> int:
    parser = argparse.ArgumentParser(description="Run and measure one LM Studio Tetris HTML task")
    parser.add_argument("--base-url", default=os.getenv("LLM_BASE_URL", "http://127.0.0.1:1234/v1"))
    parser.add_argument("--model", default=os.getenv("LLM_MODEL", ""))
    parser.add_argument("--output-dir", default="artifacts/tetris")
    args = parser.parse_args()

    base_url = args.base_url.rstrip("/")
    token = os.getenv("LM_API_TOKEN") or os.getenv("LLM_API_KEY")
    try:
        model = model_id(base_url, args.model, token)
        payload = {
            "model": model,
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": TASK_PROMPT},
            ],
            "temperature": 0.2,
            "stream": False,
        }
        started = time.perf_counter()
        response = http_json(f"{base_url}/chat/completions", payload, token)
        duration_ms = round((time.perf_counter() - started) * 1000, 3)
    except RuntimeError as error:
        print(f"tetris task failed: {error}", file=sys.stderr)
        return 1

    choices = response.get("choices", [])
    if not choices or not choices[0].get("message", {}).get("content"):
        print(f"tetris task failed: model response has no text: {response}", file=sys.stderr)
        return 1
    html = clean_html(str(choices[0]["message"]["content"]))
    usage = response.get("usage") or {}
    record = {
        "occurred_at": datetime.now(timezone.utc).isoformat(),
        "endpoint": base_url,
        "model": model,
        "duration_ms": duration_ms,
        "usage": usage,
        "usage_quality": "reported" if usage else "missing",
        "prompt_chars": len(SYSTEM_PROMPT) + len(TASK_PROMPT),
        "output_chars": len(html),
        "response_id": response.get("id", ""),
    }

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "tetris.html").write_text(html, encoding="utf-8")
    (output_dir / "tetris.run.json").write_text(
        json.dumps(record, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(f"model: {model}")
    print(f"duration_ms: {duration_ms}")
    print(f"usage: {json.dumps(usage, ensure_ascii=False)}")
    print(f"html: {output_dir / 'tetris.html'}")
    print(f"record: {output_dir / 'tetris.run.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
