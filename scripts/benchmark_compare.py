#!/usr/bin/env python3
"""Run repeated pi-golang/Pi comparisons against one OpenAI-compatible endpoint.

The benchmark deliberately uses separate, session-less processes and alternates
the runner order. It records raw JSONL where possible, then writes a compact
summary with mean/median/P95/stddev and evidence-backed optimization hints.
Only the Python standard library is required.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import shutil
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.parse
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_MODEL = "qwen3.6-35b-a3b-heretic-splash"
DEFAULT_BASE_URL = "http://127.0.0.1:1234/v1"

DEFAULT_REQUEST_PROFILE: Dict[str, Any] = {
    "temperature": {"mode": "set", "value": 0.7},
    "max_output_tokens": {"mode": "set", "value": 16384, "field": "max_completion_tokens"},
    "thinking": {"mode": "omit"},
    "extra": {},
}

USER_PROMPT = (
    "请生成一个简单但可玩的俄罗斯方块单文件 HTML 页面。只返回完整 HTML，不要 Markdown "
    "代码围栏或解释；使用内联 HTML、CSS、JavaScript，不加载外部资源；支持左右移动、软降、"
    "硬降、旋转、暂停、重新开始；显示分数、消除行数、等级、下一个方块和键盘提示。"
)


def load_text(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def run_command(
    command: Sequence[str],
    env: Dict[str, str],
    timeout: float,
) -> Tuple[int, str, str, float]:
    started = time.perf_counter()
    try:
        completed = subprocess.run(
            list(command),
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
            timeout=timeout,
            check=False,
        )
        return completed.returncode, completed.stdout, completed.stderr, (time.perf_counter() - started) * 1000
    except subprocess.TimeoutExpired as error:
        stdout = error.stdout if isinstance(error.stdout, str) else ""
        stderr = error.stderr if isinstance(error.stderr, str) else ""
        return 124, stdout, stderr + "\nbenchmark timeout", (time.perf_counter() - started) * 1000


def build_go_binary(output: Path, env: Dict[str, str], target: str = ".") -> None:
    command = ["go", "build", "-o", str(output), target]
    code, stdout, stderr, _ = run_command(command, env, timeout=120)
    if code != 0:
        raise RuntimeError(f"go build failed ({code})\nstdout:\n{stdout}\nstderr:\n{stderr}")


def load_request_profile(path: Optional[str]) -> Dict[str, Any]:
    profile = json.loads(json.dumps(DEFAULT_REQUEST_PROFILE))
    if path:
        loaded = json.loads(Path(path).read_text(encoding="utf-8"))
        if not isinstance(loaded, dict):
            raise ValueError("request profile 必须是 JSON object")
        profile.update(loaded)
    for name in ("temperature", "max_output_tokens", "thinking"):
        value = profile.get(name)
        if not isinstance(value, dict) or value.get("mode") not in {"set", "omit"}:
            raise ValueError(f"request profile.{name}.mode 必须是 set 或 omit")
    temperature = profile["temperature"]
    if temperature["mode"] == "set" and not isinstance(temperature.get("value"), (int, float)):
        raise ValueError("request profile.temperature.value 必须是数字")
    max_tokens = profile["max_output_tokens"]
    if max_tokens["mode"] == "set":
        if not isinstance(max_tokens.get("value"), int) or max_tokens["value"] < 1:
            raise ValueError("request profile.max_output_tokens.value 必须是正整数")
        if max_tokens.get("field") not in {"max_tokens", "max_completion_tokens"}:
            raise ValueError("request profile.max_output_tokens.field 必须是 max_tokens 或 max_completion_tokens")
    for name in ("extra",):
        if not isinstance(profile.get(name, {}), dict):
            raise ValueError(f"request profile.{name} 必须是 JSON object")
    thinking = profile["thinking"]
    if thinking["mode"] == "set" and not isinstance(thinking.get("extra"), dict):
        raise ValueError("request profile.thinking.extra 必须是 JSON object")
    return profile


def request_extra(profile: Dict[str, Any]) -> Dict[str, Any]:
    extra = dict(profile.get("extra", {}))
    thinking = profile["thinking"]
    if thinking["mode"] == "set":
        extra.update(thinking["extra"])
    return extra


def apply_go_request_profile(env: Dict[str, str], profile: Dict[str, Any]) -> None:
    temperature = profile["temperature"]
    env["AGENT_OMIT_TEMPERATURE"] = "true" if temperature["mode"] == "omit" else "false"
    if temperature["mode"] == "set":
        env["AGENT_TEMPERATURE"] = str(temperature["value"])
    max_tokens = profile["max_output_tokens"]
    if max_tokens["mode"] == "set":
        env["AGENT_MAX_TOKENS"] = str(max_tokens["value"])
        env["LLM_MAX_TOKENS_FIELD"] = max_tokens["field"]
    else:
        env["AGENT_MAX_TOKENS"] = "0"
        env.pop("LLM_MAX_TOKENS_FIELD", None)
    extra = request_extra(profile)
    if extra:
        env["LLM_REQUEST_EXTRA_JSON"] = json.dumps(extra, ensure_ascii=False, separators=(",", ":"))
    else:
        env.pop("LLM_REQUEST_EXTRA_JSON", None)


def write_pi_models_config(directory: Path, base_url: str, model: str, profile: Dict[str, Any]) -> None:
    temperature = profile["temperature"]
    max_tokens = profile["max_output_tokens"]
    sampling_params = request_extra(profile)
    if temperature["mode"] == "set":
        sampling_params["temperature"] = temperature["value"]
    model_config: Dict[str, Any] = {"id": model}
    if max_tokens["mode"] == "set":
        model_config["maxTokens"] = max_tokens["value"]
        model_config["compat"] = {"maxTokensField": max_tokens["field"]}
    if sampling_params:
        model_config["samplingParams"] = sampling_params
    config = {
        "providers": {
            "lmstudio": {
                "baseUrl": base_url,
                "api": "openai-completions",
                "apiKey": "lm-studio-local",
                "models": [model_config],
            }
        }
    }
    (directory / "models.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")


def unused_local_address() -> str:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return f"127.0.0.1:{sock.getsockname()[1]}"


def start_audit_proxy(binary: Path, upstream: str, audit_file: Path, log_file: Path) -> Tuple[subprocess.Popen[str], str, Any]:
    address = unused_local_address()
    log_stream = log_file.open("w", encoding="utf-8")
    process = subprocess.Popen(
        [str(binary), "--listen", address, "--upstream", upstream, "--audit-file", str(audit_file)],
        cwd=ROOT,
        text=True,
        stdout=log_stream,
        stderr=subprocess.STDOUT,
    )
    health_url = f"http://{address}/healthz"
    for _ in range(50):
        if process.poll() is not None:
            log_stream.close()
            raise RuntimeError(f"audit proxy exited early; see {log_file}")
        try:
            with urllib.request.urlopen(health_url, timeout=0.2):
                return process, f"http://{address}", log_stream
        except Exception:
            time.sleep(0.1)
    process.terminate()
    log_stream.close()
    raise RuntimeError(f"audit proxy did not become healthy; see {log_file}")


def safe_json_lines(text: str) -> Iterable[Dict[str, Any]]:
    for line in text.splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            yield value


def int_value(value: Any) -> int:
    return int(value) if isinstance(value, (int, float)) else 0


def assistant_text(message: Any) -> str:
    if isinstance(message, dict):
        content = message.get("content")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return "".join(
                str(block.get("text", ""))
                for block in content
                if isinstance(block, dict) and block.get("type") == "text"
            )
    return ""


def parse_pi_events(stdout: str) -> Dict[str, Any]:
    assistant_messages: List[Dict[str, Any]] = []
    tool_calls = 0
    errors = 0
    system_chars = 0
    for event in safe_json_lines(stdout):
        event_type = event.get("type")
        if event_type == "message_end" and isinstance(event.get("message"), dict):
            message = event["message"]
            if message.get("role") == "assistant":
                assistant_messages.append(message)
            elif message.get("role") == "system":
                system_chars += len(json.dumps(message.get("content", ""), ensure_ascii=False))
        elif event_type == "tool_execution_start":
            tool_calls += 1
        elif event_type in {"error", "agent_error"}:
            errors += 1

    usage = {"input": 0, "output": 0, "total": 0, "reasoning": 0, "cache_read": 0, "cache_write": 0}
    output_chars = 0
    for message in assistant_messages:
        raw_usage = message.get("usage") or {}
        usage["input"] += int_value(raw_usage.get("input"))
        usage["output"] += int_value(raw_usage.get("output"))
        usage["total"] += int_value(raw_usage.get("totalTokens", raw_usage.get("total")))
        usage["reasoning"] += int_value(raw_usage.get("reasoning"))
        usage["cache_read"] += int_value(raw_usage.get("cacheRead"))
        usage["cache_write"] += int_value(raw_usage.get("cacheWrite"))
        output_chars += len(assistant_text(message))

    final_text = assistant_text(assistant_messages[-1]) if assistant_messages else ""
    quality = validate_html_output(final_text)
    transport_success = bool(assistant_messages) and bool(final_text)
    return {
        "success": transport_success and quality["task_success"],
        "transport_success": transport_success,
        **quality,
        "usage": usage,
        "tool_calls": tool_calls,
        "errors": errors,
        "system_chars": system_chars,
        "output_chars": output_chars,
        "assistant_messages": len(assistant_messages),
    }


def parse_go_audit(path: Path) -> Dict[str, Any]:
    usage = {"input": 0, "output": 0, "total": 0, "reasoning": 0, "cache_read": 0, "cache_write": 0}
    request_tools = 0
    # Keep model requests and actual dispatches distinct. A malformed model
    # tool call is never an execution, even though it appeared in a response.
    model_tool_calls = 0
    tool_dispatches = 0
    errors = 0
    request_messages_chars = 0
    response_chars = 0
    records = 0
    providers: Dict[str, int] = {}
    error_classes: Dict[str, int] = {}
    http_statuses: Dict[str, int] = {}
    timeout_phases: Dict[str, int] = {}
    retry_count = 0
    protocol_failures: Dict[str, int] = {}
    recovery_strategies: Dict[str, int] = {}
    first_byte_ms = 0
    first_event_ms = 0
    first_content_ms = 0
    if path.exists():
        for record in safe_json_lines(path.read_text(encoding="utf-8")):
            records += 1
            phase = record.get("phase")
            provider = record.get("provider")
            if isinstance(provider, str) and provider:
                providers[provider] = providers.get(provider, 0) + 1
            retry_count += max(int_value(record.get("attempts")) - 1, 0)
            if phase == "tool_validation":
                reason = record.get("error_class")
                if not isinstance(reason, str) or not reason:
                    protocol = record.get("tool_protocol") or {}
                    reason = protocol.get("validation_reason") if isinstance(protocol, dict) else "unknown"
                key = reason if isinstance(reason, str) and reason else "unknown"
                protocol_failures[key] = protocol_failures.get(key, 0) + 1
                strategy = record.get("recovery_strategy")
                if isinstance(strategy, str) and strategy:
                    recovery_strategies[strategy] = recovery_strategies.get(strategy, 0) + 1
            elif phase == "tool_dispatch":
                tool_dispatches += 1
            if phase == "error":
                errors += 1
                error_class = record.get("error_class")
                if isinstance(error_class, str) and error_class:
                    error_classes[error_class] = error_classes.get(error_class, 0) + 1
                status = int_value(record.get("http_status"))
                if status:
                    key = str(status)
                    http_statuses[key] = http_statuses.get(key, 0) + 1
                timeout_phase = record.get("timeout_phase")
                if isinstance(timeout_phase, str) and timeout_phase:
                    timeout_phases[timeout_phase] = timeout_phases.get(timeout_phase, 0) + 1
            if phase in {"response", "error"}:
                for field, current in (
                    ("first_byte_ms", first_byte_ms),
                    ("first_event_ms", first_event_ms),
                    ("first_content_ms", first_content_ms),
                ):
                    value = int_value(record.get(field))
                    if value > 0 and (current == 0 or value < current):
                        if field == "first_byte_ms":
                            first_byte_ms = value
                        elif field == "first_event_ms":
                            first_event_ms = value
                        else:
                            first_content_ms = value
            request = record.get("request") or {}
            if phase == "request":
                tools = request.get("Tools") or request.get("tools") or []
                request_tools = max(request_tools, len(tools))
                request_messages_chars = max(request_messages_chars, len(json.dumps(request.get("Messages", request.get("messages", [])), ensure_ascii=False)))
            response = record.get("response") or {}
            if phase == "response":
                raw_usage = response.get("Usage") or response.get("usage") or {}
                usage["input"] += int_value(raw_usage.get("Input", raw_usage.get("input")))
                usage["output"] += int_value(raw_usage.get("Output", raw_usage.get("output")))
                usage["total"] += int_value(raw_usage.get("Total", raw_usage.get("total")))
                usage["reasoning"] += int_value(raw_usage.get("Reasoning", raw_usage.get("reasoning")))
                usage["cache_read"] += int_value(raw_usage.get("CacheRead", raw_usage.get("cache_read")))
                usage["cache_write"] += int_value(raw_usage.get("CacheWrite", raw_usage.get("cache_write")))
                calls = response.get("ToolCalls") or response.get("tool_calls") or []
                model_tool_calls += len(calls)
                content = response.get("Content", response.get("content", ""))
                response_chars += len(content) if isinstance(content, str) else 0

    return {
        "success": records > 0 and errors == 0 and usage["total"] > 0,
        "transport_success": records > 0 and errors == 0 and usage["total"] > 0,
        "usage": usage,
        "request_tools": request_tools,
        "tool_calls": tool_dispatches,
        "model_tool_calls": model_tool_calls,
        "errors": errors,
        "request_messages_chars": request_messages_chars,
        "output_chars": response_chars,
        "records": records,
        "providers": providers,
        "error_classes": error_classes,
        "http_statuses": http_statuses,
        "timeout_phases": timeout_phases,
        "retry_count": retry_count,
        "protocol_failures": protocol_failures,
        "recovery_strategies": recovery_strategies,
        "first_byte_ms": first_byte_ms,
        "first_event_ms": first_event_ms,
        "first_content_ms": first_content_ms,
    }


def validate_html_output(text: str) -> Dict[str, Any]:
    # Match the CLI artifact boundary: Markdown fences or a short preface do
    # not invalidate an otherwise complete HTML document. Compare the final
    # artifact shape rather than formatting wrappers emitted by a runner.
    value = text.strip()
    initial_lower = value.lower()
    starts = [index for index in (initial_lower.find("<!doctype html"), initial_lower.find("<html")) if index >= 0]
    if starts:
        value = value[min(starts) :]
        end = value.lower().rfind("</html>")
        if end >= 0:
            value = value[: end + len("</html>")]
    lower = value.lower()
    issues: List[str] = []
    if not (lower.startswith("<!doctype html") or lower.startswith("<html")):
        issues.append("not_single_html_document")
    if "<html" not in lower or "</html>" not in lower:
        issues.append("missing_html_root")
    if "<style" not in lower or "<script" not in lower:
        issues.append("missing_inline_assets")
    if re.search(r"(?is)<(?:script|img|link)\\b[^>]+(?:src|href)\\s*=\\s*['\"]https?://", value) or "@import url(" in lower:
        issues.append("external_resource")
    required_groups = [
        ("score", "分数"),
        ("line", "行数"),
        ("level", "等级"),
        ("next", "下一个"),
    ]
    if any(not any(marker in lower for marker in group) for group in required_groups):
        issues.append("missing_game_status")
    if "keydown" not in lower or not all(marker.lower() in lower for marker in ("ArrowLeft", "ArrowRight", "ArrowDown")):
        issues.append("missing_keyboard_controls")
    return {"task_success": not issues, "quality_issues": issues}


def validate_html_artifact(path: Path) -> Dict[str, Any]:
    if not path.exists():
        return {"task_success": False, "quality_issues": ["artifact_missing"]}
    return validate_html_output(path.read_text(encoding="utf-8"))


def parse_go_stdout(stdout: str) -> Dict[str, Any]:
    match = re.search(
        r"usage:\s+input\s+(\d+)\s+output\s+(\d+)"
        r"(?:\s+reasoning\s+\d+\s+cache_read\s+\d+\s+cache_write\s+\d+)?"
        r"\s+total\s+(\d+)\s+quality\s+(\w+)",
        stdout,
    )
    if not match:
        return {"usage_quality": "missing"}
    return {
        "usage_quality": match.group(4),
        "stdout_input": int(match.group(1)),
        "stdout_output": int(match.group(2)),
        "stdout_total": int(match.group(3)),
    }


def percentile(values: Sequence[float], fraction: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    position = (len(ordered) - 1) * fraction
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    weight = position - lower
    return ordered[lower] * (1 - weight) + ordered[upper] * weight


def summarize(records: Sequence[Dict[str, Any]], backend: str) -> Dict[str, Any]:
    selected = [record for record in records if record["backend"] == backend]
    successful = [record for record in selected if record["success"]]
    fields = ["wall_ms", "input", "output", "total", "reasoning", "cache_read", "cache_write", "tool_calls"]
    result: Dict[str, Any] = {
        "runs": len(selected),
        "successes": len(successful),
        "success_rate": round(len(successful) / len(selected), 3) if selected else 0.0,
        "basis": "successful runs only",
        "all_attempts": {},
    }
    for field in fields:
        values = [float(r[field]) for r in successful]
        attempt_values = [float(r[field]) for r in selected]
        result[field] = stats(values)
        result["all_attempts"][field] = stats(attempt_values)
    return result


def stats(values: Sequence[float]) -> Dict[str, float]:
    return {
            "mean": round(statistics.mean(values), 3) if values else 0.0,
            "median": round(statistics.median(values), 3) if values else 0.0,
            "p95": round(percentile(values, 0.95), 3),
            "stddev": round(statistics.stdev(values), 3) if len(values) > 1 else 0.0,
    }


def flatten_usage(record: Dict[str, Any]) -> None:
    usage = record.get("usage") or {}
    for field in ("input", "output", "total", "reasoning", "cache_read", "cache_write"):
        record[field] = int_value(usage.get(field))
    record.setdefault("tool_calls", 0)
    record.setdefault("model_tool_calls", record["tool_calls"])
    record.setdefault("errors", 0)
    record.setdefault("providers", {})
    record.setdefault("error_classes", {})
    record.setdefault("http_statuses", {})
    record.setdefault("timeout_phases", {})
    record.setdefault("retry_count", 0)
    record.setdefault("first_byte_ms", 0)
    record.setdefault("first_event_ms", 0)
    record.setdefault("first_content_ms", 0)


def diff_values(left: Any, right: Any, path: str = "", limit: int = 64) -> List[Dict[str, Any]]:
    if len(path) > 512:
        return [{"path": path, "go": "path_too_deep", "pi": "path_too_deep"}]
    if type(left) is not type(right):
        return [{"path": path, "go": left, "pi": right}]
    if isinstance(left, dict):
        changes: List[Dict[str, Any]] = []
        for key in sorted(set(left) | set(right)):
            if len(changes) >= limit:
                break
            child = f"{path}.{key}" if path else key
            if key not in left or key not in right:
                changes.append({"path": child, "go": left.get(key), "pi": right.get(key)})
                continue
            changes.extend(diff_values(left[key], right[key], child, limit - len(changes)))
        return changes
    if isinstance(left, list):
        changes = []
        for index in range(max(len(left), len(right))):
            if len(changes) >= limit:
                break
            child = f"{path}[{index}]"
            if index >= len(left) or index >= len(right):
                changes.append({"path": child, "go": left[index] if index < len(left) else None, "pi": right[index] if index < len(right) else None})
                continue
            changes.extend(diff_values(left[index], right[index], child, limit - len(changes)))
        return changes
    return [] if left == right else [{"path": path, "go": left, "pi": right}]


def write_payload_diffs(wire_path: Path, output_path: Path) -> Dict[str, Any]:
    records = list(safe_json_lines(wire_path.read_text(encoding="utf-8"))) if wire_path.exists() else []
    by_runner = {
        runner: [record for record in records if record.get("runner") == runner]
        for runner in ("go", "pi")
    }
    pairs = min(len(by_runner["go"]), len(by_runner["pi"]))
    exact_matches = 0
    generation_matches = 0
    with output_path.open("w", encoding="utf-8") as stream:
        for index in range(pairs):
            go_record = by_runner["go"][index]
            pi_record = by_runner["pi"][index]
            go_semantic = go_record.get("semantic", {})
            pi_semantic = pi_record.get("semantic", {})
            generation_keys = ("stream", "stream_options", "temperature", "max_tokens", "max_completion_tokens", "top_p", "seed", "reasoning_effort", "thinking", "chat_template_kwargs")
            go_generation = {key: go_semantic.get(key) for key in generation_keys if key in go_semantic}
            pi_generation = {key: pi_semantic.get(key) for key in generation_keys if key in pi_semantic}
            exact = go_record.get("canonical_sha256") == pi_record.get("canonical_sha256")
            generation_equal = go_generation == pi_generation
            exact_matches += int(exact)
            generation_matches += int(generation_equal)
            stream.write(json.dumps({
                "pair": index + 1,
                "go_sequence": go_record.get("sequence"),
                "pi_sequence": pi_record.get("sequence"),
                "exact_payload_equal": exact,
                "generation_fields_equal": generation_equal,
                "generation_diff": diff_values(go_generation, pi_generation),
                "semantic_diff": diff_values(go_semantic, pi_semantic),
            }, ensure_ascii=False) + "\n")
    return {
        "go_requests": len(by_runner["go"]),
        "pi_requests": len(by_runner["pi"]),
        "paired_requests": pairs,
        "exact_payload_matches": exact_matches,
        "generation_field_matches": generation_matches,
        "unpaired_requests": len(by_runner["go"]) + len(by_runner["pi"]) - pairs * 2,
    }


def relative(go_value: float, pi_value: float) -> float:
    return round((go_value - pi_value) / pi_value * 100, 2) if pi_value else 0.0


def count_breakdown(records: Sequence[Dict[str, Any]], field: str, backend: str = "pi-golang") -> Dict[str, int]:
    totals: Dict[str, int] = {}
    for record in records:
        if record.get("backend") != backend:
            continue
        values = record.get(field) or {}
        if not isinstance(values, dict):
            continue
        for key, value in values.items():
            totals[str(key)] = totals.get(str(key), 0) + int_value(value)
    return dict(sorted(totals.items()))


def optimization_hints(records: Sequence[Dict[str, Any]]) -> List[str]:
    hints: List[str] = []
    go_all = [r for r in records if r["backend"] == "pi-golang"]
    go = [r for r in records if r["backend"] == "pi-golang" and r["success"]]
    pi = [r for r in records if r["backend"] == "pi" and r["success"]]
    if go_all and all(r.get("request_tools", 0) > 0 for r in go_all) and all(r.get("tool_calls", 0) == 0 for r in go_all):
            hints.append("纯 HTML/文本任务中工具调用为 0，但 Go 每轮仍携带工具 schema；本项目应使用 --tools=disabled 做单因素对比，可减少输入上下文和模型决策分支。")
    elif any(r.get("tool_calls", 0) > 0 for r in go_all):
            hints.append("工具开启样本出现实际 tool call；应把工具调用成功率、参数解析失败和后续 provider 500 分开统计，纯生成任务应显式使用 --tools=disabled。")
    if go and pi:
        go_output = statistics.mean(r["output"] for r in go)
        pi_output = statistics.mean(r["output"] for r in pi)
        if go_output > pi_output * 1.1:
            hints.append("Go 输出 token 的均值明显高于 Pi；先固定 thinking/temperature，再增加 max output token 或 HTML 结构约束，避免模型过度解释/思考。")
        go_wall = statistics.mean(r["wall_ms"] for r in go)
        pi_wall = statistics.mean(r["wall_ms"] for r in pi)
        if go_wall > pi_wall * 1.1 and abs(relative(go_output, pi_output)) < 10:
            hints.append("耗时差异大于输出 token 差异；结合 first_byte_ms/first_event_ms/first_content_ms 区分连接、模型排队和完整生成耗时，再排查 HTTP client 与连接复用。")
        go_attempts = len(go_all)
        pi_attempts = len([r for r in records if r["backend"] == "pi"])
        if go_attempts and pi_attempts and len(go) / go_attempts < len(pi) / pi_attempts:
            hints.append(f"Go 成功率为 {len(go)}/{go_attempts}，低于 Pi 的 {len(pi)}/{pi_attempts}；先按错误类型拆分 500、超时、协议/工具参数错误，再优化性能。")
    if any(r["reasoning"] == 0 for r in go) and any(r["reasoning"] > 0 for r in pi):
        hints.append("Pi 已上报 reasoning token，而 Go 当前聚合为 0；补齐 completion_tokens_details.reasoning_tokens 与 prompt cache 字段，才能区分‘模型思考多’和‘可见输出多’。")
    if any(r["errors"] for r in records):
        hints.append("存在失败样本；把 HTTP 状态、超时阶段和重试次数作为低基数指标，并按 model/provider 分组，不要只看成功请求平均值。")
    if not hints:
        hints.append("本轮没有触发自动优化规则；继续观察 P95、失败样本和工具调用分布，再决定是否调整 prompt、工具 profile 或传输层。")
    return hints


def write_report(path: Path, args: argparse.Namespace, records: Sequence[Dict[str, Any]]) -> None:
    go_summary = summarize(records, "pi-golang")
    pi_summary = summarize(records, "pi")
    lines = [
        "# pi-golang vs Pi repeated benchmark",
        "",
        f"- 时间：{datetime.now(timezone.utc).isoformat()}",
        f"- 样本：Go {go_summary['runs']} 次，Pi {pi_summary['runs']} 次；每次独立 session",
        f"- Endpoint：`{args.base_url}`",
        f"- Model：`{args.model}`",
        f"- 任务：相同俄罗斯方块单文件 HTML prompt；Pi {'携带内置工具' if args.pi_tools else '禁用内置工具'}，Go {'携带默认工具' if args.go_tools else '使用 --no-tools'}",
        "- 注意：这是本机 LM Studio 的重复样本，不是跨机器基准；模型缓存、温度和服务负载仍会影响结果。",
        "",
        "## 汇总",
        "",
        "主表只统计成功运行；失败次数和失败请求的部分 token 不进入性能均值。",
        "",
        "| 指标（均值） | pi-golang | Pi | Go 相对 Pi |",
        "|---|---:|---:|---:|",
    ]
    wire_summary = getattr(args, "wire_summary", None)
    if isinstance(wire_summary, dict):
        lines.extend([
            f"- Wire 审计：Go `{wire_summary['go_requests']}` 个请求，Pi `{wire_summary['pi_requests']}` 个请求；"
            f"已配对 `{wire_summary['paired_requests']}`，生成参数一致 `{wire_summary['generation_field_matches']}/{wire_summary['paired_requests']}`。",
            "- `wire-requests.jsonl` 和 `payload-diffs.jsonl` 仅含脱敏字段、长度与 SHA-256，不写入 prompt 或 Authorization。",
        ])
    for label, field in [("耗时 ms", "wall_ms"), ("输入 token", "input"), ("输出 token", "output"), ("总 token", "total"), ("reasoning token", "reasoning"), ("工具调用", "tool_calls")]:
        go_mean = go_summary[field]["mean"]
        pi_mean = pi_summary[field]["mean"]
        display_label = "工具执行/调度（成功样本）" if label == "工具调用" else label
        lines.append(f"| {display_label} | {go_mean:.3f} | {pi_mean:.3f} | {relative(go_mean, pi_mean):+.2f}% |")
    lines.extend(
        [
            f"| 成功率 | {go_summary['success_rate'] * 100:.1f}% | {pi_summary['success_rate'] * 100:.1f}% | — |",
            f"| 工具调用（全部尝试均值） | {go_summary['all_attempts']['tool_calls']['mean']:.3f} | {pi_summary['all_attempts']['tool_calls']['mean']:.3f} | — |",
        ]
    )
    go_timing = {
        field: stats([float(record.get(field, 0)) for record in records if record.get("backend") == "pi-golang" and int_value(record.get(field)) > 0])
        for field in ("first_byte_ms", "first_event_ms", "first_content_ms")
    }
    if any(value["mean"] > 0 for value in go_timing.values()):
        lines.extend(
            [
                "",
                "Go 首响应时间（成功/失败审计中有值的调用）",
                "",
                f"- first_byte_ms：`{go_timing['first_byte_ms']['mean']:.3f}` 平均，P95 `{go_timing['first_byte_ms']['p95']:.3f}`",
                f"- first_event_ms：`{go_timing['first_event_ms']['mean']:.3f}` 平均，P95 `{go_timing['first_event_ms']['p95']:.3f}`",
                f"- first_content_ms：`{go_timing['first_content_ms']['mean']:.3f}` 平均，P95 `{go_timing['first_content_ms']['p95']:.3f}`",
            ]
        )
    go_model_tool_calls = sum(
        int_value(record.get("model_tool_calls"))
        for record in records
        if record.get("backend") == "pi-golang"
    )
    if go_model_tool_calls:
        go_tool_dispatches = sum(
            int_value(record.get("tool_calls"))
            for record in records
            if record.get("backend") == "pi-golang"
        )
        lines.append(
            f"- Go 模型工具调用：`{go_model_tool_calls}`；其中实际调度：`{go_tool_dispatches}`。"
            "参数校验拒绝的调用不计为执行。"
        )
    for backend, label in (("pi-golang", "Go"), ("pi", "Pi")):
        samples = [record for record in records if record.get("backend") == backend and "task_success" in record]
        if samples:
            task_successes = sum(1 for record in samples if record.get("task_success"))
            lines.append(f"- {label} HTML 质量通过率：`{task_successes}/{len(samples)}`（静态结构检查）")
    error_classes = count_breakdown(records, "error_classes")
    http_statuses = count_breakdown(records, "http_statuses")
    timeout_phases = count_breakdown(records, "timeout_phases")
    protocol_failures = count_breakdown(records, "protocol_failures")
    recovery_strategies = count_breakdown(records, "recovery_strategies")
    retry_count = sum(int_value(record.get("retry_count")) for record in records if record.get("backend") == "pi-golang")
    lines.extend(
        [
            "",
            "## Go 失败分类（全部尝试）",
            "",
            f"- provider：`{json.dumps(count_breakdown(records, 'providers'), ensure_ascii=False)}`",
            f"- error_class：`{json.dumps(error_classes, ensure_ascii=False)}`",
            f"- HTTP status：`{json.dumps(http_statuses, ensure_ascii=False)}`",
            f"- timeout phase：`{json.dumps(timeout_phases, ensure_ascii=False)}`",
            f"- tool protocol failure：`{json.dumps(protocol_failures, ensure_ascii=False)}`",
            f"- recovery strategy：`{json.dumps(recovery_strategies, ensure_ascii=False)}`",
            f"- 自动重试次数：`{retry_count}`（当前 adapter 不隐式重试；此字段用于审计未来显式重试策略）",
        ]
    )
    lines.extend(
        [
            "",
            "## 稳定性（P50 / P95 / 标准差）",
            "",
            "```json",
            json.dumps({"pi-golang": go_summary, "pi": pi_summary}, ensure_ascii=False, indent=2),
            "```",
            "",
            "## 审计驱动的优化路径",
            "",
        ]
    )
    lines.extend(f"{index}. {hint}" for index, hint in enumerate(optimization_hints(records), 1))
    lines.extend(
        [
            "",
            "## 原始记录",
            "",
            "- `records.jsonl`：每次运行的 wall time、usage、错误和工具计数",
            "- `request-profile.json`：本轮 Go/Pi 共同使用的请求参数配置（新基准运行时生成）",
            "- `go/`：Go Agent 的 JSONL 审计（默认脱敏）",
            "- `pi/`：Pi JSON event stream",
        ]
    )
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Run repeated pi-golang/Pi local LLM comparisons")
    parser.add_argument("--runs", type=int, default=20, help="每个 runner 的次数，默认 20")
    parser.add_argument("--base-url", default=os.getenv("LLM_BASE_URL", DEFAULT_BASE_URL))
    parser.add_argument("--model", default=os.getenv("LLM_MODEL", DEFAULT_MODEL))
    parser.add_argument("--output-dir", default="artifacts/comparison/benchmark-20")
    parser.add_argument("--timeout", type=float, default=180.0, help="单次运行超时秒数")
    parser.add_argument("--smoke", action="store_true", help="只跑每个 runner 1 次")
    parser.add_argument("--go-tools", action="store_true", help="Go Agent 也携带默认工具；默认关闭以进行纯文本公平比较")
    parser.add_argument("--pi-tools", action="store_true", help="Pi 也开启内置工具；与 --go-tools 一起使用可测试真实工具循环")
    parser.add_argument("--request-profile", help="共享请求 profile JSON，显式对齐 temperature、token 上限和 thinking 字段")
    parser.add_argument("--wire-audit", action="store_true", help="经本地透明代理记录 Go/Pi 脱敏 wire request 并生成 payload diff")
    parser.add_argument("--go-only", action="store_true", help="只运行 Go Agent，不启动 Pi")
    parser.add_argument("--compare-pi-records", help="go-only 模式下复用已有 records.jsonl 中的 Pi 记录作为基线")
    parser.add_argument("--report-only", help="只用已有 records.jsonl 生成报告，不调用 LLM")
    args = parser.parse_args()
    if args.smoke:
        args.runs = 1
    if args.runs < 1:
        parser.error("--runs 必须 >= 1")
    if args.go_only and not args.compare_pi_records:
        parser.error("--go-only 必须同时提供 --compare-pi-records")
    try:
        request_profile = load_request_profile(args.request_profile)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        parser.error(f"request profile 无效: {error}")

    if args.report_only:
        records_path = Path(args.report_only).resolve()
        if not records_path.exists():
            parser.error(f"records 文件不存在: {records_path}")
        records = [value for value in safe_json_lines(records_path.read_text(encoding="utf-8"))]
        for record in records:
            flatten_usage(record)
        # A go-only comparison may reuse a longer Pi baseline. Reports should
        # compare equally sized samples; preserve the source JSONL unchanged.
        go_count = sum(1 for record in records if record.get("backend") == "pi-golang")
        if go_count:
            baseline_pi = [record for record in records if record.get("backend") == "pi" and record.get("source")]
            if len(baseline_pi) > go_count:
                kept_pi_ids = {id(record) for record in baseline_pi[:go_count]}
                records = [
                    record
                    for record in records
                    if record.get("backend") != "pi" or not record.get("source") or id(record) in kept_pi_ids
                ]
        # Recompute protocol counters from the per-run audit when available.
        # This keeps report-only mode correct after the schema evolves.
        for record in records:
            if record.get("backend") != "pi-golang":
                continue
            run = int_value(record.get("run"))
            if run < 1:
                continue
            audit = parse_go_audit(records_path.parent / "go" / f"run-{run:03d}.jsonl")
            if audit["records"]:
                for field in ("tool_calls", "model_tool_calls", "protocol_failures", "recovery_strategies"):
                    record[field] = audit[field]
        # Pi stores the raw event stream. Re-parse it so report-only mode also
        # benefits from parser and artifact-quality fixes added after a run.
        for record in records:
            if record.get("backend") != "pi":
                continue
            run = int_value(record.get("run"))
            if run < 1:
                continue
            raw_path = records_path.parent / "pi" / f"run-{run:03d}.jsonl"
            if not raw_path.exists():
                continue
            parsed = parse_pi_events(raw_path.read_text(encoding="utf-8"))
            record.update(parsed)
            flatten_usage(record)
        # Success has the same contract for every runner: transport completed,
        # the requested HTML artifact passed static validation, and the command
        # itself exited successfully.
        for record in records:
            record["success"] = (
                bool(record.get("transport_success", record.get("success")))
                and bool(record.get("task_success", True))
                and int_value(record.get("exit_code")) == 0
            )
        args.output_dir = str(records_path.parent)
        args.runs = max((int(record.get("run", 0)) for record in records), default=0)
        args.go_tools = any(int(record.get("request_tools", 0)) > 0 for record in records if record.get("backend") == "pi-golang")
        report_path = records_path.parent / "report.md"
        write_report(report_path, args, records)
        (records_path.parent / "summary.json").write_text(
            json.dumps({"pi-golang": summarize(records, "pi-golang"), "pi": summarize(records, "pi")}, ensure_ascii=False, indent=2)
            + "\n",
            encoding="utf-8",
        )
        print(f"report: {report_path}")
        return 0

    pi_path = shutil.which("pi")
    if not args.go_only and not pi_path:
        print("未找到 pi CLI，请先安装 pi", file=sys.stderr)
        return 2

    output_dir = (ROOT / args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "request-profile.json").write_text(
        json.dumps(request_profile, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    (output_dir / "go").mkdir(exist_ok=True)
    if not args.go_only:
        (output_dir / "pi").mkdir(exist_ok=True)
    records_path = output_dir / "records.jsonl"
    if records_path.exists():
        records_path.unlink()
    env = os.environ.copy()
    env["LLM_BASE_URL"] = args.base_url
    env["LLM_MODEL"] = args.model
    env.setdefault("LM_API_TOKEN", "")
    env["GOCACHE"] = "/tmp/pi-golang-benchmark-gocache"
    apply_go_request_profile(env, request_profile)

    with tempfile.TemporaryDirectory(prefix="pi-golang-benchmark-") as temp_dir:
        temp_root = Path(temp_dir)
        binary = temp_root / "pi-agent"
        build_go_binary(binary, env)
        go_base_url = args.base_url
        pi_base_url = args.base_url
        audit_process: Optional[subprocess.Popen[str]] = None
        audit_log = None
        if args.wire_audit:
            audit_binary = temp_root / "llm-audit-proxy"
            build_go_binary(audit_binary, env, "./cmd/llm-audit-proxy")
            parsed_base_url = urllib.parse.urlsplit(args.base_url)
            upstream = urllib.parse.urlunsplit((parsed_base_url.scheme, parsed_base_url.netloc, "", "", ""))
            base_path = parsed_base_url.path.rstrip("/")
            audit_process, proxy_base_url, audit_log = start_audit_proxy(
                audit_binary,
                upstream,
                output_dir / "wire-requests.jsonl",
                output_dir / "audit-proxy.log",
            )
            go_base_url = f"{proxy_base_url}/go{base_path}"
            pi_base_url = f"{proxy_base_url}/pi{base_path}"
        pi_agent_dir = temp_root / "pi-agent-dir"
        pi_agent_dir.mkdir()
        write_pi_models_config(pi_agent_dir, pi_base_url, args.model, request_profile)
        pi_env = dict(env)
        pi_env["PI_CODING_AGENT_DIR"] = str(pi_agent_dir)
        pi_env["PI_OFFLINE"] = "1"
        system_prompt = load_text(ROOT / "internal/prompt/base.md")

        records: List[Dict[str, Any]] = []
        if args.compare_pi_records:
            baseline_path = Path(args.compare_pi_records).resolve()
            if not baseline_path.exists():
                parser.error(f"Pi 基线 records 文件不存在: {baseline_path}")
            baseline = [value for value in safe_json_lines(baseline_path.read_text(encoding="utf-8"))]
            baseline_pi = [record for record in baseline if record.get("backend") == "pi"][: args.runs]
            if not baseline_pi:
                parser.error(f"Pi 基线不包含 backend=pi 记录: {baseline_path}")
            for record in baseline_pi:
                flatten_usage(record)
                record["source"] = str(baseline_path)
            records.extend(baseline_pi)
            with records_path.open("a", encoding="utf-8") as stream:
                for record in baseline_pi:
                    stream.write(json.dumps(record, ensure_ascii=False) + "\n")
        for index in range(1, args.runs + 1):
            order = ("pi-golang",) if args.go_only else (("pi-golang", "pi") if index % 2 else ("pi", "pi-golang"))
            for backend in order:
                print(f"[{index}/{args.runs}] {backend}", flush=True)
                raw_dir = output_dir / backend.replace("-", "_")
                raw_dir.mkdir(exist_ok=True)
                if backend == "pi-golang":
                    audit_path = output_dir / "go" / f"run-{index:03d}.jsonl"
                    command = [
                        str(binary),
                        "run",
                        "--provider",
                        "lmstudio",
                        "--base-url",
                        go_base_url,
                        "--model",
                        args.model,
                        "--prompt",
                        USER_PROMPT,
                        "--audit-file",
                        str(audit_path),
                        "--output",
                        str(output_dir / "go" / f"run-{index:03d}.html"),
                    ]
                    if args.go_tools:
                        # --output 会触发 CLI 的 auto 策略；显式 enabled 才能
                        # 在本轮真实验证 LM Studio 的工具续轮协议。
                        command.extend(["--tools", "enabled"])
                    else:
                        command.append("--no-tools")
                    code, stdout, stderr, wall_ms = run_command(command, env, args.timeout)
                    parsed = parse_go_audit(audit_path)
                    parsed.update(parse_go_stdout(stdout))
                    parsed.update(validate_html_artifact(output_dir / "go" / f"run-{index:03d}.html"))
                    parsed["backend"] = backend
                    parsed["run"] = index
                    parsed["wall_ms"] = round(wall_ms, 3)
                    parsed["exit_code"] = code
                    parsed["stderr_tail"] = stderr[-1000:]
                elif not args.go_only:
                    raw_path = output_dir / "pi" / f"run-{index:03d}.jsonl"
                    command = [
                        pi_path,
                        "--provider",
                        "lmstudio",
                        "--model",
                        args.model,
                        "--api-key",
                        "lm-studio-local",
                        "--no-session",
                        "--no-context-files",
                        "--no-skills",
                        "--no-prompt-templates",
                        "--no-extensions",
                        "--offline",
                        "--thinking",
                        "off",
                        "--system-prompt",
                        system_prompt,
                        "--mode",
                        "json",
                        "--print",
                        "--",
                        USER_PROMPT,
                    ]
                    if not args.pi_tools:
                        command.insert(command.index("--no-context-files"), "--no-tools")
                    code, stdout, stderr, wall_ms = run_command(command, pi_env, args.timeout)
                    raw_path.write_text(stdout, encoding="utf-8")
                    parsed = parse_pi_events(stdout)
                    parsed["backend"] = backend
                    parsed["run"] = index
                    parsed["wall_ms"] = round(wall_ms, 3)
                    parsed["exit_code"] = code
                    parsed["stderr_tail"] = stderr[-1000:]

                parsed["success"] = (
                    bool(parsed.get("transport_success", parsed.get("success")))
                    and bool(parsed.get("task_success", True))
                    and code == 0
                )
                flatten_usage(parsed)
                with records_path.open("a", encoding="utf-8") as stream:
                    stream.write(json.dumps(parsed, ensure_ascii=False) + "\n")
                records.append(parsed)

        if audit_process is not None:
            audit_process.terminate()
            try:
                audit_process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                audit_process.kill()
            if audit_log is not None:
                audit_log.close()

    if args.wire_audit:
        args.wire_summary = write_payload_diffs(output_dir / "wire-requests.jsonl", output_dir / "payload-diffs.jsonl")

    report_path = output_dir / "report.md"
    write_report(report_path, args, records)
    (output_dir / "summary.json").write_text(
        json.dumps({"pi-golang": summarize(records, "pi-golang"), "pi": summarize(records, "pi")}, ensure_ascii=False, indent=2)
        + "\n",
        encoding="utf-8",
    )
    print(f"report: {report_path}")
    return 0 if all(record["success"] for record in records) else 1


if __name__ == "__main__":
    raise SystemExit(main())
