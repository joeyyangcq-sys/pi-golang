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
import statistics
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_MODEL = "qwen3.6-35b-a3b-heretic-splash"
DEFAULT_BASE_URL = "http://127.0.0.1:1234/v1"

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


def build_go_binary(output: Path, env: Dict[str, str]) -> None:
    command = ["go", "build", "-o", str(output), "."]
    code, stdout, stderr, _ = run_command(command, env, timeout=120)
    if code != 0:
        raise RuntimeError(f"go build failed ({code})\nstdout:\n{stdout}\nstderr:\n{stderr}")


def write_pi_models_config(directory: Path, base_url: str, model: str) -> None:
    config = {
        "providers": {
            "lmstudio": {
                "baseUrl": base_url,
                "api": "openai-completions",
                "apiKey": "lm-studio-local",
                "models": [{"id": model}],
            }
        }
    }
    (directory / "models.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")


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

    success = bool(assistant_messages) and bool(assistant_text(assistant_messages[-1]))
    return {
        "success": success,
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
    tool_calls = 0
    errors = 0
    request_messages_chars = 0
    response_chars = 0
    records = 0
    if path.exists():
        for record in safe_json_lines(path.read_text(encoding="utf-8")):
            records += 1
            phase = record.get("phase")
            if phase == "error":
                errors += 1
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
                tool_calls += len(calls)
                content = response.get("Content", response.get("content", ""))
                response_chars += len(content) if isinstance(content, str) else 0

    return {
        "success": records > 0 and errors == 0 and usage["total"] > 0,
        "usage": usage,
        "request_tools": request_tools,
        "tool_calls": tool_calls,
        "errors": errors,
        "request_messages_chars": request_messages_chars,
        "output_chars": response_chars,
        "records": records,
    }


def parse_go_stdout(stdout: str) -> Dict[str, Any]:
    match = re.search(r"usage:\s+input\s+(\d+)\s+output\s+(\d+)\s+total\s+(\d+)\s+quality\s+(\w+)", stdout)
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
    fields = ["wall_ms", "input", "output", "total", "reasoning", "cache_read", "cache_write", "tool_calls"]
    result: Dict[str, Any] = {"runs": len(selected), "successes": sum(1 for r in selected if r["success"])}
    for field in fields:
        values = [float(r[field]) for r in selected]
        result[field] = {
            "mean": round(statistics.mean(values), 3) if values else 0.0,
            "median": round(statistics.median(values), 3) if values else 0.0,
            "p95": round(percentile(values, 0.95), 3),
            "stddev": round(statistics.stdev(values), 3) if len(values) > 1 else 0.0,
        }
    return result


def relative(go_value: float, pi_value: float) -> float:
    return round((go_value - pi_value) / pi_value * 100, 2) if pi_value else 0.0


def optimization_hints(records: Sequence[Dict[str, Any]]) -> List[str]:
    hints: List[str] = []
    go = [r for r in records if r["backend"] == "pi-golang" and r["success"]]
    pi = [r for r in records if r["backend"] == "pi" and r["success"]]
    if go and all(r["request_tools"] > 0 and r["tool_calls"] == 0 for r in go):
        hints.append("纯 HTML/文本任务中工具调用为 0，但 Go 每轮仍携带工具 schema；增加任务 profile，在无需工具时关闭工具，可减少输入上下文和模型决策分支。")
    if go and pi:
        go_output = statistics.mean(r["output"] for r in go)
        pi_output = statistics.mean(r["output"] for r in pi)
        if go_output > pi_output * 1.1:
            hints.append("Go 输出 token 的均值明显高于 Pi；先固定 thinking/temperature，再增加 max output token 或 HTML 结构约束，避免模型过度解释/思考。")
        go_wall = statistics.mean(r["wall_ms"] for r in go)
        pi_wall = statistics.mean(r["wall_ms"] for r in pi)
        if go_wall > pi_wall * 1.1 and abs(relative(go_output, pi_output)) < 10:
            hints.append("耗时差异大于输出 token 差异；重点排查 HTTP client、连接复用、首 token 等待和 JSON 序列化，给每次 LLM 调用增加 request_sent/first_byte/completed 时间点。")
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
        f"- 次数：每个 runner {args.runs} 次，交替顺序，独立 session",
        f"- Endpoint：`{args.base_url}`",
        f"- Model：`{args.model}`",
        "- 任务：相同俄罗斯方块单文件 HTML prompt；Pi 禁用内置工具，Go 使用当前默认工具注册",
        "- 注意：这是本机 LM Studio 的重复样本，不是跨机器基准；模型缓存、温度和服务负载仍会影响结果。",
        "",
        "## 汇总",
        "",
        "| 指标（均值） | pi-golang | Pi | Go 相对 Pi |",
        "|---|---:|---:|---:|",
    ]
    for label, field in [("耗时 ms", "wall_ms"), ("输入 token", "input"), ("输出 token", "output"), ("总 token", "total"), ("reasoning token", "reasoning"), ("工具调用", "tool_calls")]:
        go_mean = go_summary[field]["mean"]
        pi_mean = pi_summary[field]["mean"]
        lines.append(f"| {label} | {go_mean:.3f} | {pi_mean:.3f} | {relative(go_mean, pi_mean):+.2f}% |")
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
    args = parser.parse_args()
    if args.smoke:
        args.runs = 1
    if args.runs < 1:
        parser.error("--runs 必须 >= 1")

    pi_path = shutil.which("pi")
    if not pi_path:
        print("未找到 pi CLI，请先安装 pi", file=sys.stderr)
        return 2

    output_dir = (ROOT / args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "go").mkdir(exist_ok=True)
    (output_dir / "pi").mkdir(exist_ok=True)
    env = os.environ.copy()
    env["LLM_BASE_URL"] = args.base_url
    env["LLM_MODEL"] = args.model
    env.setdefault("LM_API_TOKEN", "")
    env["GOCACHE"] = "/tmp/pi-golang-benchmark-gocache"

    with tempfile.TemporaryDirectory(prefix="pi-golang-benchmark-") as temp_dir:
        temp_root = Path(temp_dir)
        binary = temp_root / "pi-agent"
        build_go_binary(binary, env)
        pi_agent_dir = temp_root / "pi-agent-dir"
        pi_agent_dir.mkdir()
        write_pi_models_config(pi_agent_dir, args.base_url, args.model)
        pi_env = dict(env)
        pi_env["PI_CODING_AGENT_DIR"] = str(pi_agent_dir)
        pi_env["PI_OFFLINE"] = "1"
        system_prompt = load_text(ROOT / "internal/prompt/base.md")

        records: List[Dict[str, Any]] = []
        for index in range(1, args.runs + 1):
            order = ("pi-golang", "pi") if index % 2 else ("pi", "pi-golang")
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
                        args.base_url,
                        "--model",
                        args.model,
                        "--prompt",
                        USER_PROMPT,
                        "--audit-file",
                        str(audit_path),
                        "--output",
                        str(output_dir / "go" / f"run-{index:03d}.html"),
                    ]
                    code, stdout, stderr, wall_ms = run_command(command, env, args.timeout)
                    parsed = parse_go_audit(audit_path)
                    parsed.update(parse_go_stdout(stdout))
                    parsed["backend"] = backend
                    parsed["run"] = index
                    parsed["wall_ms"] = round(wall_ms, 3)
                    parsed["exit_code"] = code
                    parsed["stderr_tail"] = stderr[-1000:]
                else:
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
                        "--no-tools",
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
                    code, stdout, stderr, wall_ms = run_command(command, pi_env, args.timeout)
                    raw_path.write_text(stdout, encoding="utf-8")
                    parsed = parse_pi_events(stdout)
                    parsed["backend"] = backend
                    parsed["run"] = index
                    parsed["wall_ms"] = round(wall_ms, 3)
                    parsed["exit_code"] = code
                    parsed["stderr_tail"] = stderr[-1000:]

                record_path = output_dir / "records.jsonl"
                with record_path.open("a", encoding="utf-8") as stream:
                    stream.write(json.dumps(parsed, ensure_ascii=False) + "\n")
                records.append(parsed)

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
