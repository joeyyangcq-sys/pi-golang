#!/usr/bin/env python3
"""Safely replay an audited OpenAI-compatible tool continuation.

The input must be a *full-content* pi-golang JSONL audit. The script never
executes local tools; it only rebuilds one HTTP chat-completions request so a
developer can vary the wire shape that an LM Studio chat template receives.
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Dict, Iterable


def json_lines(path: Path) -> Iterable[Dict[str, Any]]:
    for line in path.read_text(encoding="utf-8").splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            yield value


def choose_record(path: Path, iteration: int) -> Dict[str, Any]:
    for record in json_lines(path):
        if record.get("phase") == "request" and record.get("iteration") == iteration:
            return record
    raise ValueError(f"未找到 iteration={iteration} 的 request 审计记录")


def reject_redacted(value: Any) -> None:
    encoded = json.dumps(value, ensure_ascii=False)
    if "[redacted chars=" in encoded:
        raise ValueError("审计内容已脱敏；请只在受控本地环境使用 --audit-content full 重新采样")


def to_wire_payload(record: Dict[str, Any], assistant_content: str, tools_mode: str) -> Dict[str, Any]:
    request = record.get("request") or {}
    reject_redacted(request)
    messages = []
    for message in request.get("Messages", request.get("messages", [])):
        role = message.get("role", message.get("Role"))
        wire: Dict[str, Any] = {"role": role}
        calls = message.get("ToolCalls", message.get("tool_calls", [])) or []
        content = message.get("content", message.get("Content", ""))
        if role == "assistant" and calls:
            if assistant_content == "null":
                wire["content"] = None
            elif assistant_content == "preserve" and content:
                wire["content"] = content
            elif assistant_content == "omit":
                pass
            else:
                wire["content"] = None
            wire["tool_calls"] = [
                {
                    "id": call.get("ID", call.get("id")),
                    "type": "function",
                    "function": {
                        "name": call.get("Name", call.get("name")),
                        "arguments": call.get("Arguments", call.get("arguments")),
                    },
                }
                for call in calls
            ]
        else:
            wire["content"] = content
        call_id = message.get("ToolCallID", message.get("tool_call_id"))
        if call_id:
            wire["tool_call_id"] = call_id
        messages.append(wire)

    payload: Dict[str, Any] = {
        "model": request.get("Model", request.get("model")),
        "messages": messages,
        "temperature": request.get("Temperature", request.get("temperature", 0.7)),
    }
    if tools_mode == "preserve":
        infos = request.get("Tools", request.get("tools", [])) or []
        if infos:
            payload["tools"] = [
                {
                    "type": "function",
                    "function": {
                        "name": info.get("name", info.get("Name")),
                        "description": info.get("description", info.get("Description", "")),
                        "parameters": info.get("input_schema", info.get("InputSchema", {"type": "object"})),
                    },
                }
                for info in infos
            ]
    return payload


def main() -> int:
    parser = argparse.ArgumentParser(description="Replay one audited tool-continuation HTTP payload without executing tools")
    parser.add_argument("--audit-record", required=True, type=Path, help="full-content JSONL audit file")
    parser.add_argument("--iteration", required=True, type=int, help="request iteration to replay")
    parser.add_argument("--base-url", default="http://127.0.0.1:1234/v1")
    parser.add_argument("--assistant-content", choices=("preserve", "null", "omit"), default="preserve")
    parser.add_argument("--tools", choices=("preserve", "omit"), default="omit")
    parser.add_argument("--output", type=Path, help="write the request/response pair to this file")
    parser.add_argument("--dry-run", action="store_true", help="print payload only; do not send HTTP")
    args = parser.parse_args()

    try:
        record = choose_record(args.audit_record, args.iteration)
        payload = to_wire_payload(record, args.assistant_content, args.tools)
    except (OSError, ValueError) as error:
        print(f"replay preparation failed: {error}", file=sys.stderr)
        return 2

    if args.dry_run:
        print(json.dumps(payload, ensure_ascii=False, indent=2))
        return 0

    endpoint = args.base_url.rstrip("/") + "/chat/completions"
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            body = response.read().decode("utf-8", errors="replace")
            result = {"request": payload, "status": response.status, "response": body}
    except urllib.error.HTTPError as error:
        result = {"request": payload, "status": error.code, "response": error.read().decode("utf-8", errors="replace")}
    except OSError as error:
        print(f"replay failed: {error}", file=sys.stderr)
        return 1

    rendered = json.dumps(result, ensure_ascii=False, indent=2)
    if args.output:
        args.output.write_text(rendered + "\n", encoding="utf-8")
    else:
        print(rendered)
    return 0 if 200 <= result["status"] < 300 else 1


if __name__ == "__main__":
    raise SystemExit(main())
