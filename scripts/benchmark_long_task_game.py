#!/usr/bin/env python3
"""Run one long coding task through pi-golang and Pi with compaction evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Sequence, Tuple

import benchmark_compare as common


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_PROFILE = ROOT / "scripts" / "request-profile.lmstudio-seed42.json"
CONTEXT_WINDOW = 14000
CONTEXT_RESERVE = 5000
CONTEXT_KEEP_RECENT = 3000
CONTEXT_SUMMARY_MAX = 1200
CONTEXT_TOOL_RESULT_MAX = 1600

TASK_PROMPT = """这是一个长 coding 任务。请在当前空工作区交付一个可运行的浏览器游戏，并持续使用工具完成全部阶段，不要只给方案后停止。

主题：制作一个以战锤 40,000 氛围为背景的原创单人第一人称风格波次防守射击游戏。可以使用原创的帝国卫士、混沌入侵者等文字和程序化图形，但不要下载或引用任何远程图片、字体、脚本、官方 logo 或现成游戏代码。

必须严格按以下顺序推进：
1. 规划阶段：先创建 PLAN.md，写出目标、玩家循环、文件架构、状态模型、输入设计、验收标准和分阶段实现计划。PLAN.md 控制在 120 行以内；创建 PLAN.md 后再创建其他源文件。
2. 实现阶段：创建 index.html、styles.css、game.js、README.md。实现一个无需构建工具、直接用浏览器打开即可运行的游戏，至少包含 canvas 或等效战场、鼠标瞄准/射击、键盘移动、弹药和换弹、生命值、敌人生成与命中、波次、分数、暂停、重开、HUD 和清晰的视觉反馈。保持实现精简，game.js 不超过约 300 行或 12 KiB，避免单次工具参数超过模型输出预算。
3. 验证阶段：运行 node --check game.js，并检查 HTML 的本地资源引用；如果检查或运行中发现问题，继续修改并再次验证。
4. 收尾阶段：回读关键文件，确认 PLAN.md 与实现一致，补充 README.md 的运行方式和操作说明。最终回答包含已完成文件、验证命令和仍存在的限制。

实现质量要求：代码结构清晰，避免外部依赖；在没有图片素材时用 canvas、CSS、渐变、几何图形和文字建立深色科幻战场；游戏必须有明确的胜负/波次反馈和可操作性。敌人从待生成队列进入活动列表的路径必须完整；接触伤害必须有基于时间的受击冷却，不能按每个动画帧连续扣血。每个 write/edit 工具调用只处理一个文件，避免在一个超长工具调用里写多个文件。请实际写入文件并验证，不要声称执行过没有执行的命令。"""

REQUIRED_FILES = ("PLAN.md", "index.html", "styles.css", "game.js", "README.md")


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def write_pi_long_config(directory: Path, base_url: str, model: str, profile: Dict[str, Any]) -> Dict[str, str]:
    directory.mkdir(parents=True, exist_ok=True)
    temperature = profile["temperature"]
    max_tokens = profile["max_output_tokens"]
    sampling = common.request_extra(profile)
    if temperature["mode"] == "set":
        sampling["temperature"] = temperature["value"]
    model_config: Dict[str, Any] = {
        "id": model,
        "contextWindow": CONTEXT_WINDOW,
    }
    if max_tokens["mode"] == "set":
        model_config["maxTokens"] = max_tokens["value"]
        model_config["compat"] = {"maxTokensField": max_tokens["field"]}
    if sampling:
        model_config["samplingParams"] = sampling
    models = {
        "providers": {
            "lmstudio": {
                "baseUrl": base_url,
                "api": "openai-completions",
                "apiKey": "lm-studio-local",
                "models": [model_config],
            }
        }
    }
    (directory / "models.json").write_text(json.dumps(models, indent=2) + "\n", encoding="utf-8")
    settings = {
        "compaction": {
            "enabled": True,
            "reserveTokens": CONTEXT_RESERVE,
            "keepRecentTokens": CONTEXT_KEEP_RECENT,
        }
    }
    (directory / "settings.json").write_text(json.dumps(settings, indent=2) + "\n", encoding="utf-8")
    env = os.environ.copy()
    env["PI_CODING_AGENT_DIR"] = str(directory)
    env["PI_OFFLINE"] = "1"
    return env


def prepare_workspace(path: Path) -> None:
    if path.exists():
        shutil.rmtree(path)
    path.mkdir(parents=True)


def validate_game(workspace: Path) -> Dict[str, Any]:
    issues: List[str] = []
    files: Dict[str, int] = {}
    for name in REQUIRED_FILES:
        target = workspace / name
        if not target.exists() or not target.is_file() or target.stat().st_size == 0:
            issues.append(f"missing_or_empty:{name}")
        else:
            files[name] = target.stat().st_size

    plan = (workspace / "PLAN.md").read_text(encoding="utf-8", errors="replace") if (workspace / "PLAN.md").exists() else ""
    plan_lower = plan.lower()
    required_plan_sections = (
        ("目标", "goal"),
        ("架构", "architecture"),
        ("验收", "acceptance"),
    )
    if not all(any(marker in plan_lower for marker in alternatives) for alternatives in required_plan_sections):
        issues.append("plan_missing_required_sections")

    html = (workspace / "index.html").read_text(encoding="utf-8", errors="replace") if (workspace / "index.html").exists() else ""
    css = (workspace / "styles.css").read_text(encoding="utf-8", errors="replace") if (workspace / "styles.css").exists() else ""
    js = (workspace / "game.js").read_text(encoding="utf-8", errors="replace") if (workspace / "game.js").exists() else ""
    combined = "\n".join((html, css, js, plan))
    if re.search(r"https?://|//cdn\.|fonts\.googleapis", combined, re.IGNORECASE):
        issues.append("external_resource_reference")
    if not re.search(r"<script[^>]+src=[\"'](?:\./)?game\.js", html, re.IGNORECASE):
        issues.append("index_missing_game_script")
    if not re.search(r"<link[^>]+href=[\"'](?:\./)?styles\.css", html, re.IGNORECASE):
        issues.append("index_missing_stylesheet")
    markers = {
        "animation_loop": r"requestAnimationFrame|setInterval",
        "keyboard_input": r"keydown|keyup",
        "mouse_input": r"pointerdown|mousedown|mousemove",
        "shooting": r"shoot|fire|bullet|projectile",
        "reload": r"reload|弹药|换弹",
        "enemy_wave": r"enemy|敌人|wave|波次",
        "score_health": r"score|分数|health|生命",
    }
    for label, marker in markers.items():
        if not re.search(marker, js, re.IGNORECASE):
            issues.append(f"game_js_missing:{label}")
    if re.search(r"spawnQueue|spawn_queue", js, re.IGNORECASE) and not re.search(
        r"enemies\s*\.\s*(?:push|unshift)\s*\(|enemies\s*=\s*enemies\s*\.\s*concat",
        js,
        re.IGNORECASE,
    ):
        issues.append("spawn_queue_never_activates_enemies")
    if not re.search(r"invulner|damage[_A-Za-z]*cooldown|hit[_A-Za-z]*cooldown|last[_A-Za-z]*damage|next[_A-Za-z]*damage", js, re.IGNORECASE):
        issues.append("missing_contact_damage_cooldown")

    code, stdout, stderr, test_ms = common.run_command(
        ["node", "--check", "game.js"], os.environ.copy(), timeout=30, cwd=workspace
    )
    if code != 0:
        issues.append("node_check_failed")
    return {
        "task_success": not issues,
        "quality_issues": issues,
        "files": files,
        "node_check_exit_code": code,
        "node_check_ms": round(test_ms, 3),
        "node_check_stdout": stdout[-1000:],
        "node_check_stderr": stderr[-1000:],
        "artifact_sha256": sha256_text("\n".join(
            (workspace / name).read_text(encoding="utf-8", errors="replace")
            for name in REQUIRED_FILES if (workspace / name).exists()
        )),
    }


def extract_pi_answer(events: Iterable[Dict[str, Any]]) -> str:
    answer = ""
    for event in events:
        if event.get("type") != "message_end":
            continue
        message = event.get("message")
        if isinstance(message, dict) and message.get("role") == "assistant":
            answer = common.assistant_text(message)
    return answer.strip()


def parse_pi_events(stdout: str) -> Dict[str, Any]:
    events = list(common.safe_json_lines(stdout))
    compaction_start = [event for event in events if event.get("type") == "compaction_start"]
    compaction_end = [event for event in events if event.get("type") == "compaction_end"]
    errors = [event for event in events if event.get("type") in {"error", "agent_error"}]
    tool_starts = [event for event in events if event.get("type") == "tool_execution_start"]
    assistant_messages = [
        event for event in events
        if event.get("type") == "message_end"
        and isinstance(event.get("message"), dict)
        and event["message"].get("role") == "assistant"
    ]
    usage = {"input": 0, "output": 0, "total": 0, "reasoning": 0, "cache_read": 0, "cache_write": 0}
    for event in assistant_messages:
        raw = event["message"].get("usage") or {}
        usage["input"] += common.int_value(raw.get("input"))
        usage["output"] += common.int_value(raw.get("output"))
        usage["total"] += common.int_value(raw.get("totalTokens", raw.get("total")))
        usage["reasoning"] += common.int_value(raw.get("reasoning"))
        usage["cache_read"] += common.int_value(raw.get("cacheRead"))
        usage["cache_write"] += common.int_value(raw.get("cacheWrite"))
    compactions: List[Dict[str, Any]] = []
    for event in compaction_end:
        result = event.get("result") or {}
        summary = result.get("summary") if isinstance(result, dict) else ""
        compactions.append({
            "reason": event.get("reason"),
            "aborted": event.get("aborted", False),
            "will_retry": event.get("willRetry", False),
            "tokens_before": result.get("tokensBefore") if isinstance(result, dict) else None,
            "summary_chars": len(summary) if isinstance(summary, str) else 0,
            "summary_sha256": sha256_text(summary) if isinstance(summary, str) and summary else "",
            "usage": result.get("usage") if isinstance(result, dict) else {},
        })
    return {
        "events": len(events),
        "assistant_messages": len(assistant_messages),
        "tool_calls": len(tool_starts),
        "errors": len(errors),
        "compaction_starts": len(compaction_start),
        "compaction_ends": len(compaction_end),
        "compaction_count": len([item for item in compactions if not item.get("aborted")]),
        "compactions": compactions,
        "usage": usage,
        "answer": extract_pi_answer(events),
    }


def parse_go_run(audit_path: Path, stdout: str) -> Dict[str, Any]:
    parsed = common.parse_go_audit(audit_path)
    records = list(common.safe_json_lines(audit_path.read_text(encoding="utf-8"))) if audit_path.exists() else []
    phases = {phase: sum(1 for record in records if record.get("phase") == phase) for phase in (
        "request", "response", "error", "tool_dispatch", "tool_validation", "compaction_request", "compaction_response", "compaction_error"
    )}
    compactions = []
    for record in records:
        if record.get("phase") != "compaction_response":
            continue
        compaction = record.get("compaction") or {}
        response = record.get("response") or {}
        compactions.append({
            "estimated_tokens": compaction.get("estimated_tokens"),
            "trigger_tokens": compaction.get("trigger_tokens"),
            "source_messages": compaction.get("source_messages"),
            "retained_messages": compaction.get("retained_messages"),
            "compacted_until": compaction.get("compacted_until"),
            "summary_chars": compaction.get("summary_chars"),
            "summary_sha256": compaction.get("summary_sha256"),
            "fallback": compaction.get("fallback", False),
            "fallback_reason": compaction.get("fallback_reason", ""),
            "usage": response.get("Usage") or response.get("usage") or {},
        })
    match = re.search(r"compactions:\s+(\d+)", stdout)
    conversations_match = re.search(r"conversations:\s+(\d+)", stdout)
    legacy_agent_runs_match = re.search(r"agent_runs:\s+(\d+)", stdout)
    agent_turns_match = re.search(r"agent_turns:\s+(\d+)", stdout)
    plan_tasks_match = re.search(r"plan_tasks:\s+(\d+)", stdout)
    replans_match = re.search(r"replans:\s+(\d+)", stdout)
    orchestration_id_match = re.search(r"orchestration_id:\s+(\S+)", stdout)
    role_counts: Dict[str, int] = {}
    worker_tasks = set()
    for record in records:
        if record.get("phase") != "request":
            continue
        role = record.get("orchestration_role")
        if role:
            role_counts[role] = role_counts.get(role, 0) + 1
        if role == "worker" and record.get("orchestration_task"):
            worker_tasks.add(record["orchestration_task"])
    parsed.update({
        "conversations": int(conversations_match.group(1)) if conversations_match else int(legacy_agent_runs_match.group(1)) if legacy_agent_runs_match else 1,
        "agent_turns": int(agent_turns_match.group(1)) if agent_turns_match else int(legacy_agent_runs_match.group(1)) if legacy_agent_runs_match else 1,
        "plan_tasks": int(plan_tasks_match.group(1)) if plan_tasks_match else 0,
        "replans": int(replans_match.group(1)) if replans_match else 0,
        "orchestration_id": orchestration_id_match.group(1) if orchestration_id_match else "",
        "orchestration_role_requests": role_counts,
        "worker_task_ids": sorted(worker_tasks),
        "phases": phases,
        "compaction_count_stdout": int(match.group(1)) if match else 0,
        "compaction_count": len(compactions),
        "compaction_fallbacks": sum(1 for item in compactions if item.get("fallback")),
        "compactions": compactions,
    })
    return parsed


def audit_wire_summary(path: Path) -> Dict[str, Any]:
    rows = list(common.safe_json_lines(path.read_text(encoding="utf-8"))) if path.exists() else []
    result: Dict[str, Any] = {}
    for runner in ("go-long", "pi-long"):
        selected = [row for row in rows if row.get("runner") == runner]
        messages = [(row.get("semantic") or {}).get("messages") or [] for row in selected]
        result[runner] = {
            "requests": len(selected),
            "max_messages": max((len(value) for value in messages), default=0),
            "requests_with_tools": sum(1 for row in selected if (row.get("semantic") or {}).get("tools")),
            "unique_payload_shapes": len({
                json.dumps({key: (row.get("semantic") or {}).get(key) for key in ("messages", "tools", "temperature", "max_completion_tokens", "seed")}, sort_keys=True)
                for row in selected
            }),
        }
    return result


def write_report(path: Path, args: argparse.Namespace, records: Sequence[Dict[str, Any]], wire: Dict[str, Any]) -> None:
    lines = [
        "# Long task game validation",
        "",
        f"- Generated: {datetime.now(timezone.utc).isoformat()}",
        f"- Endpoint: `{args.base_url}`",
        f"- Model: `{args.model}`",
        f"- Context window: `{CONTEXT_WINDOW}`; reserve `{CONTEXT_RESERVE}`; keep recent `{CONTEXT_KEEP_RECENT}`.",
        "- Task: plan first, then implement and validate an original Warhammer 40,000 themed browser defense shooter.",
        "",
        "## Runner results",
        "",
        "| Runner | exit | task | conversations | agent turns | plan tasks | replans | tools | compactions | fallbacks | compaction phases | wall ms | input | output | reasoning |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|",
    ]
    for record in records:
        usage = record.get("usage") or {}
        phases = record.get("phases") or {}
        lines.append(
            f"| {record['runner']} | {record['exit_code']} | {'pass' if record['task_success'] else 'FAIL'} | "
            f"{record.get('conversations', 1)} | {record.get('agent_turns', record.get('conversations', 1))} | "
            f"{record.get('plan_tasks', 0)} | {record.get('replans', 0)} | "
            f"{record.get('tool_calls', 0)} | {record.get('compaction_count', 0)} | {record.get('compaction_fallbacks', 0)} | "
            f"{phases.get('compaction_request', record.get('compaction_starts', 0))}/"
            f"{phases.get('compaction_response', record.get('compaction_ends', 0))}/"
            f"{phases.get('compaction_error', 0)} | "
            f"{record['wall_ms']:.1f} | {usage.get('input', 0)} | {usage.get('output', 0)} | {usage.get('reasoning', 0)} |"
        )
    lines.extend([
        "",
        "Phase order is request/response/error for Go compaction; Pi reports compaction_start/end events. A completed compaction is counted only when a summary result is present.",
        "",
        "## Task validation",
        "",
    ])
    for record in records:
        lines.append(f"- `{record['runner']}`: `{'pass' if record['task_success'] else 'FAIL'}`; issues: `{', '.join(record.get('quality_issues', [])) or 'none'}`.")
        if record.get("orchestration_role_requests"):
            lines.append(
                f"  Orchestration ID: `{record.get('orchestration_id', '')}`; requests: "
                f"`{json.dumps(record['orchestration_role_requests'], sort_keys=True)}`; "
                f"worker tasks: `{', '.join(record.get('worker_task_ids', [])) or 'none'}`."
            )
    lines.extend([
        "",
        "## Audit capability check",
        "",
        "- Go: JSONL includes every request/response, tool dispatch, compaction trigger/result/error/fallback, token usage, request shape, summary length/hash and compaction boundary.",
        "- Pi: JSON event stream includes tool executions and compaction start/end; compaction result exposes tokens before, summary usage and summary text hash in this runner.",
        "- Shared proxy: both runners have redacted wire payload shape, message count, tool presence and generation-field evidence.",
        "- Limitation: Pi and Go use different tool schemas and internal token estimators; compare completion and direction of compaction behavior, not exact per-message token equality.",
        "",
        "## Wire summary",
        "",
        "```json",
        json.dumps(wire, ensure_ascii=False, indent=2),
        "```",
        "",
        "## Artifacts",
        "",
        "- `records.jsonl`: normalized runner metrics and validation evidence.",
        "- `wire-requests.jsonl`: redacted requests captured by the local proxy.",
        "- `go/audit.jsonl`: Go audit events.",
        "- `pi/events.jsonl`: Pi JSON event stream.",
        "- `go/workspace/` and `pi/workspace/`: final generated projects.",
    ])
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Run a long game coding task through pi-golang and Pi")
    parser.add_argument("--base-url", default=os.getenv("LLM_BASE_URL", common.DEFAULT_BASE_URL))
    parser.add_argument("--model", default=os.getenv("LLM_MODEL", common.DEFAULT_MODEL))
    parser.add_argument("--request-profile", default=str(DEFAULT_PROFILE))
    parser.add_argument("--output-dir", default="artifacts/comparison/long-task-game")
    parser.add_argument("--timeout", type=float, default=1200.0)
    args = parser.parse_args()
    pi_path = shutil.which("pi")
    if not pi_path:
        parser.error("未找到 pi CLI")
    profile = common.load_request_profile(args.request_profile)
    output_dir = (ROOT / args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "request-profile.json").write_text(json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (output_dir / "task-prompt.md").write_text(TASK_PROMPT + "\n", encoding="utf-8")
    records_path = output_dir / "records.jsonl"
    wire_path = output_dir / "wire-requests.jsonl"
    for path in (records_path, wire_path):
        if path.exists():
            path.unlink()

    base_env = os.environ.copy()
    base_env["GOCACHE"] = "/tmp/pi-golang-long-task-gocache"
    common.apply_go_request_profile(base_env, profile)
    base_env.update({
        "AGENT_INCLUDE_WORKING_DIRECTORY": "true",
        "LLM_USER_CONTENT_FORMAT": "parts",
        "AGENT_CONTEXT_WINDOW": str(CONTEXT_WINDOW),
        "AGENT_CONTEXT_RESERVE_TOKENS": str(CONTEXT_RESERVE),
        "AGENT_CONTEXT_KEEP_RECENT_TOKENS": str(CONTEXT_KEEP_RECENT),
        "AGENT_CONTEXT_SUMMARY_MAX_TOKENS": str(CONTEXT_SUMMARY_MAX),
        "AGENT_CONTEXT_TOOL_RESULT_MAX_CHARS": str(CONTEXT_TOOL_RESULT_MAX),
        "AGENT_MAX_ITERATIONS": "24",
        "AGENT_TIMEOUT": "20m",
    })

    records: List[Dict[str, Any]] = []
    with tempfile.TemporaryDirectory(prefix="pi-golang-long-task-") as temp_dir:
        temp_root = Path(temp_dir)
        binary = temp_root / "pi-agent"
        proxy_binary = temp_root / "llm-audit-proxy"
        common.build_go_binary(binary, base_env)
        common.build_go_binary(proxy_binary, base_env, "./cmd/llm-audit-proxy")
        parsed_url = urllib.parse.urlsplit(args.base_url)
        upstream = urllib.parse.urlunsplit((parsed_url.scheme, parsed_url.netloc, "", "", ""))
        api_path = parsed_url.path.rstrip("/") or "/v1"
        proxy, proxy_base_url, proxy_log = common.start_audit_proxy(
            proxy_binary, upstream, wire_path, output_dir / "audit-proxy.log"
        )
        go_dir = output_dir / "go"
        pi_dir = output_dir / "pi"
        prepare_workspace(go_dir / "workspace")
        prepare_workspace(pi_dir / "workspace")
        go_audit = go_dir / "audit.jsonl"
        if go_audit.exists():
            go_audit.unlink()
        go_env = dict(base_env)
        go_command = [
            str(binary), "run", "--provider", "lmstudio", "--base-url", f"{proxy_base_url}/go-long{api_path}",
            "--model", args.model, "--prompt", TASK_PROMPT, "--audit-file", str(go_audit),
            "--tools", "enabled", "--task-profile", "agent-mutation", "--orchestration", "plan",
            "--max-plan-tasks", "16", "--protocol-attempts", "4", "--worker-attempts", "1",
            "--max-replans", "2",
        ]
        try:
            go_code, go_stdout, go_stderr, go_wall = common.run_command(go_command, go_env, args.timeout, cwd=go_dir / "workspace")
            go_validation = validate_game(go_dir / "workspace")
            go_parsed = parse_go_run(go_audit, go_stdout)
            go_record = {
                "runner": "pi-golang", "exit_code": go_code, "wall_ms": round(go_wall, 3),
                "task_success": go_code == 0 and bool(go_validation["task_success"]),
                "quality_issues": go_validation["quality_issues"], "validation": go_validation,
                "stderr_tail": go_stderr[-2000:], "stdout_tail": go_stdout[-2000:],
                **go_parsed,
            }
            records.append(go_record)
            (go_dir / "stdout.txt").write_text(go_stdout, encoding="utf-8")
            (go_dir / "stderr.txt").write_text(go_stderr, encoding="utf-8")

            pi_env = write_pi_long_config(temp_root / "pi-config", f"{proxy_base_url}/pi-long{api_path}", args.model, profile)
            pi_command = [
                pi_path, "--provider", "lmstudio", "--model", args.model, "--api-key", "lm-studio-local",
                "--no-session", "--no-context-files", "--no-skills", "--no-prompt-templates", "--no-extensions",
                "--offline", "--thinking", "off", "--tools", "read,bash,edit,write,find,grep,ls",
                "--system-prompt", common.load_text(ROOT / "internal/prompt/base.md"),
                "--mode", "json", "--print", "--", TASK_PROMPT,
            ]
            pi_code, pi_stdout, pi_stderr, pi_wall = common.run_command(pi_command, pi_env, args.timeout, cwd=pi_dir / "workspace")
            pi_events = parse_pi_events(pi_stdout)
            pi_validation = validate_game(pi_dir / "workspace")
            pi_record = {
                "runner": "pi", "exit_code": pi_code, "wall_ms": round(pi_wall, 3),
                "task_success": pi_code == 0 and bool(pi_validation["task_success"]),
                "quality_issues": pi_validation["quality_issues"], "validation": pi_validation,
                "stderr_tail": pi_stderr[-2000:], "stdout_tail": pi_stdout[-2000:],
                **pi_events,
            }
            records.append(pi_record)
            (pi_dir / "events.jsonl").write_text(pi_stdout, encoding="utf-8")
            (pi_dir / "stderr.txt").write_text(pi_stderr, encoding="utf-8")
        finally:
            proxy.terminate()
            try:
                proxy.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proxy.kill()
            proxy_log.close()

    with records_path.open("w", encoding="utf-8") as stream:
        for record in records:
            stream.write(json.dumps(record, ensure_ascii=False) + "\n")
    wire = audit_wire_summary(wire_path)
    (output_dir / "wire-validation.json").write_text(json.dumps(wire, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    report = output_dir / "report.md"
    write_report(report, args, records, wire)
    print(f"report: {report}")
    return 0 if all(record["task_success"] for record in records) else 1


if __name__ == "__main__":
    raise SystemExit(main())
