#!/usr/bin/env python3
"""Validate the <cwd> prompt effect on a coding task and a plain text task.

Each task runs three cells: Go without cwd, Go with cwd, and Pi. Coding runs
operate on a freshly recreated isolated fixture and are accepted only when the
file changed, tests pass, and the test file stayed untouched. Text runs disable
tools and are accepted by deterministic structure/content checks.
"""

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

CODING_PROMPT = """请修复当前工作区中的 calculator.py。
先读取 calculator.py 和 test_calculator.py，再修复 clamp 函数：合法区间必须把值限制在 minimum 和 maximum 之间；minimum 大于 maximum 时仍应抛出 ValueError。
请实际修改 calculator.py，不要修改测试文件，不要增加依赖。完成后简要说明修改。"""

TEXT_PROMPT = """用简体中文解释 HTTP 幂等性。严格只输出 5 条编号列表，每条一行，总长度不超过 500 个中文字符。
内容必须覆盖 GET、PUT、POST 的差异，网络重试，以及幂等键的用途。不要写标题、代码块或额外总结。"""

FIXTURE = {
    "calculator.py": '''def clamp(value: int, minimum: int, maximum: int) -> int:\n    """Return value constrained to the inclusive range."""\n    if minimum > maximum:\n        raise ValueError("minimum must not exceed maximum")\n    return min(minimum, max(maximum, value))\n''',
    "test_calculator.py": '''import unittest\n\nfrom calculator import clamp\n\n\nclass ClampTests(unittest.TestCase):\n    def test_value_below_range(self):\n        self.assertEqual(clamp(-4, 0, 10), 0)\n\n    def test_value_inside_range(self):\n        self.assertEqual(clamp(6, 0, 10), 6)\n\n    def test_value_above_range(self):\n        self.assertEqual(clamp(14, 0, 10), 10)\n\n    def test_invalid_range(self):\n        with self.assertRaises(ValueError):\n            clamp(5, 10, 0)\n\n\nif __name__ == "__main__":\n    unittest.main()\n''',
}

GROUPS = (
    {"id": "go-base", "backend": "pi-golang", "cwd": False},
    {"id": "go-cwd", "backend": "pi-golang", "cwd": True},
    {"id": "pi", "backend": "pi", "cwd": True},
)


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def reset_fixture(workspace: Path) -> Dict[str, str]:
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir(parents=True)
    for name, content in FIXTURE.items():
        (workspace / name).write_text(content, encoding="utf-8")
    return {name: sha256_text(content) for name, content in FIXTURE.items()}


def go_answer(stdout: str) -> str:
    marker = "answer:\n"
    if marker not in stdout:
        return ""
    return stdout.split(marker, 1)[1].strip()


def pi_answer(stdout: str) -> str:
    answer = ""
    for event in common.safe_json_lines(stdout):
        if event.get("type") != "message_end":
            continue
        message = event.get("message")
        if isinstance(message, dict) and message.get("role") == "assistant":
            answer = common.assistant_text(message)
    return answer.strip()


def validate_text(answer: str) -> Dict[str, Any]:
    lines = [line.strip() for line in answer.splitlines() if line.strip()]
    numbers = [
        match.group(1)
        for line in lines
        if (match := re.match(r"^(?:\*\*)?([1-5])[.、)]", line))
    ]
    missing = [term for term in ("GET", "PUT", "POST", "重试", "幂等键") if term not in answer]
    issues: List[str] = []
    if len(lines) != 5 or numbers != ["1", "2", "3", "4", "5"]:
        issues.append("not_exactly_five_numbered_items")
    if missing:
        issues.append("missing:" + ",".join(missing))
    if len(answer) > 500:
        issues.append("over_500_chars")
    if "```" in answer:
        issues.append("contains_code_fence")
    return {"task_success": not issues, "quality_issues": issues}


def validate_coding(workspace: Path, initial_hashes: Dict[str, str], env: Dict[str, str]) -> Dict[str, Any]:
    calculator = workspace / "calculator.py"
    tests = workspace / "test_calculator.py"
    code, stdout, stderr, test_ms = common.run_command(
        [sys.executable, "-m", "unittest", "-v"], env, timeout=30, cwd=workspace
    )
    calculator_text = calculator.read_text(encoding="utf-8") if calculator.exists() else ""
    tests_text = tests.read_text(encoding="utf-8") if tests.exists() else ""
    file_changed = bool(calculator_text) and sha256_text(calculator_text) != initial_hashes["calculator.py"]
    tests_unchanged = bool(tests_text) and sha256_text(tests_text) == initial_hashes["test_calculator.py"]
    issues: List[str] = []
    if not file_changed:
        issues.append("calculator_not_changed")
    if not tests_unchanged:
        issues.append("tests_changed")
    if code != 0:
        issues.append("unit_tests_failed")
    return {
        "task_success": not issues,
        "quality_issues": issues,
        "file_changed": file_changed,
        "tests_unchanged": tests_unchanged,
        "test_exit_code": code,
        "test_ms": round(test_ms, 3),
        "test_stdout": stdout[-1000:],
        "test_stderr": stderr[-1000:],
        "artifact_sha256": sha256_text(calculator_text) if calculator_text else "",
    }


def write_pi_config(directory: Path, base_url: str, model: str, profile: Dict[str, Any]) -> Dict[str, str]:
    directory.mkdir(parents=True, exist_ok=True)
    common.write_pi_models_config(directory, base_url, model, profile)
    env = os.environ.copy()
    env["PI_CODING_AGENT_DIR"] = str(directory)
    env["PI_OFFLINE"] = "1"
    return env


def run_go(
    binary: Path,
    task: str,
    group: Dict[str, Any],
    run: int,
    workspace: Path,
    output_dir: Path,
    env: Dict[str, str],
    proxy_base_url: str,
    api_path: str,
    args: argparse.Namespace,
) -> Tuple[Dict[str, Any], str]:
    raw_dir = output_dir / task / group["id"]
    raw_dir.mkdir(parents=True, exist_ok=True)
    audit = raw_dir / f"run-{run:03d}.jsonl"
    if audit.exists():
        audit.unlink()
    local_env = dict(env)
    local_env["AGENT_INCLUDE_WORKING_DIRECTORY"] = "true" if group["cwd"] else "false"
    local_env["LLM_USER_CONTENT_FORMAT"] = "text"
    prompt = CODING_PROMPT if task == "coding" else TEXT_PROMPT
    command = [
        str(binary), "run", "--provider", "lmstudio",
        "--base-url", f"{proxy_base_url}/{group['id']}-{task}{api_path}",
        "--model", args.model, "--prompt", prompt,
        "--audit-file", str(audit),
    ]
    if task == "coding":
        command.extend(["--tools", "enabled", "--task-profile", "agent-mutation"])
    else:
        command.extend(["--no-tools", "--task-profile", "generation"])
    code, stdout, stderr, wall_ms = common.run_command(command, local_env, args.timeout, cwd=workspace)
    parsed = common.parse_go_audit(audit)
    parsed.update(common.parse_go_stdout(stdout))
    answer = go_answer(stdout)
    parsed.update({
        "backend": "pi-golang", "group": group["id"], "task": task, "run": run,
        "cwd": group["cwd"], "wall_ms": round(wall_ms, 3), "exit_code": code,
        "stderr_tail": stderr[-1000:], "answer_sha256": sha256_text(answer) if answer else "",
    })
    return parsed, answer


def run_pi(
    pi_path: str,
    task: str,
    run: int,
    workspace: Path,
    output_dir: Path,
    pi_env: Dict[str, str],
    args: argparse.Namespace,
) -> Tuple[Dict[str, Any], str]:
    raw_dir = output_dir / task / "pi"
    raw_dir.mkdir(parents=True, exist_ok=True)
    raw_path = raw_dir / f"run-{run:03d}.jsonl"
    prompt = CODING_PROMPT if task == "coding" else TEXT_PROMPT
    command = [
        pi_path, "--provider", "lmstudio", "--model", args.model,
        "--api-key", "lm-studio-local", "--no-session", "--no-context-files",
        "--no-skills", "--no-prompt-templates", "--no-extensions", "--offline",
        "--thinking", "off", "--system-prompt", common.load_text(ROOT / "internal/prompt/base.md"),
        "--mode", "json", "--print", "--", prompt,
    ]
    if task == "text":
        command.insert(command.index("--no-context-files"), "--no-tools")
    code, stdout, stderr, wall_ms = common.run_command(command, pi_env, args.timeout, cwd=workspace)
    raw_path.write_text(stdout, encoding="utf-8")
    parsed = common.parse_pi_events(stdout)
    answer = pi_answer(stdout)
    parsed.update({
        "backend": "pi", "group": "pi", "task": task, "run": run, "cwd": True,
        "wall_ms": round(wall_ms, 3), "exit_code": code, "stderr_tail": stderr[-1000:],
        "answer_sha256": sha256_text(answer) if answer else "", "first_content_ms": 0,
    })
    return parsed, answer


def finish_record(record: Dict[str, Any], validation: Dict[str, Any]) -> Dict[str, Any]:
    record.update(validation)
    common.flatten_usage(record)
    record["visible_output"] = max(record["output"] - record["reasoning"], 0)
    record["success"] = (
        bool(record.get("transport_success", record.get("success")))
        and bool(record.get("task_success"))
        and int(record.get("exit_code", 1)) == 0
    )
    return record


def stats(records: Sequence[Dict[str, Any]], task: str, group: str, field: str) -> Dict[str, float]:
    values = [
        float(r.get(field, 0))
        for r in records
        if r["task"] == task and r["group"] == group and r.get("transport_success")
    ]
    return common.stats(values)


def summarize(records: Sequence[Dict[str, Any]]) -> Dict[str, Any]:
    result: Dict[str, Any] = {}
    for task in ("coding", "text"):
        result[task] = {}
        for group in ("go-base", "go-cwd", "pi"):
            selected = [r for r in records if r["task"] == task and r["group"] == group]
            artifacts = {
                artifact
                for record in selected
                if record["success"]
                for artifact in [record.get("artifact_sha256") or record.get("answer_sha256")]
                if artifact
            }
            result[task][group] = {
                "runs": len(selected),
                "successes": sum(1 for r in selected if r["success"]),
                **{field: stats(records, task, group, field) for field in (
                    "wall_ms", "input", "output", "reasoning", "visible_output", "first_content_ms"
                )},
                "unique_artifacts": len(artifacts),
            }
    return result


def initial_wire_summary(path: Path) -> Dict[str, Any]:
    records = list(common.safe_json_lines(path.read_text(encoding="utf-8"))) if path.exists() else []
    result: Dict[str, Any] = {}
    for task in ("coding", "text"):
        for group in ("go-base", "go-cwd", "pi"):
            runner = f"{group}-{task}" if group != "pi" else f"pi-{task}"
            selected = []
            all_runner_records = []
            for record in records:
                if record.get("runner") != runner:
                    continue
                all_runner_records.append(record)
                messages = (record.get("semantic") or {}).get("messages") or []
                if len(messages) == 2:
                    selected.append(record)
            system_bytes = set()
            user_kinds = set()
            generation = set()
            for record in selected:
                semantic = record.get("semantic") or {}
                messages = semantic.get("messages") or []
                content = messages[0].get("content") if messages and isinstance(messages[0], dict) else None
                if isinstance(content, dict):
                    system_bytes.add(content.get("bytes"))
                user = messages[1].get("content") if len(messages) > 1 and isinstance(messages[1], dict) else None
                user_kinds.add("parts" if isinstance(user, list) else "text")
                fields = {key: semantic.get(key) for key in (
                    "stream", "stream_options", "temperature", "max_completion_tokens", "seed", "store"
                ) if key in semantic}
                generation.add(json.dumps(fields, sort_keys=True))
            continuation = [
                record
                for record in all_runner_records
                if len((record.get("semantic") or {}).get("messages") or []) > 2
            ]
            result[f"{task}/{group}"] = {
                "all_requests": len(all_runner_records),
                "initial_requests": len(selected),
                "continuation_requests": len(continuation),
                "continuations_with_tools": sum(
                    1 for record in continuation if (record.get("semantic") or {}).get("tools")
                ),
                "system_bytes": sorted(x for x in system_bytes if isinstance(x, int)),
                "user_content_kinds": sorted(user_kinds), "unique_generation_shapes": len(generation),
            }
    return result


def write_report(path: Path, args: argparse.Namespace, records: Sequence[Dict[str, Any]], wire: Dict[str, Any]) -> None:
    summary = summarize(records)
    lines = [
        "# CWD generalization validation",
        "",
        f"- Generated: {datetime.now(timezone.utc).isoformat()}",
        f"- Endpoint: `{args.base_url}`",
        f"- Model: `{args.model}`",
        f"- Runs per task/cell: `{args.runs}`; order rotates by task and round.",
        "- Cells: Go base, Go + cwd, Pi reference.",
        "",
    ]
    for task, title in (("coding", "Coding mutation"), ("text", "Plain text")):
        lines.extend([
            f"## {title}", "",
            "| Cell | success | wall ms | reasoning | visible output | first content ms | input | unique artifact |",
            "|---|---:|---:|---:|---:|---:|---:|---:|",
        ])
        for group, label in (("go-base", "Go base"), ("go-cwd", "Go + cwd"), ("pi", "Pi reference")):
            item = summary[task][group]
            first = "n/a" if group == "pi" else f"{item['first_content_ms']['mean']:.1f}"
            lines.append(
                f"| {label} | {item['successes']}/{item['runs']} | {item['wall_ms']['mean']:.1f} | "
                f"{item['reasoning']['mean']:.1f} | {item['visible_output']['mean']:.1f} | {first} | "
                f"{item['input']['mean']:.1f} | {item['unique_artifacts']} |"
            )
        base = summary[task]["go-base"]
        cwd = summary[task]["go-cwd"]
        lines.extend([
            "",
            f"Go cwd effect: reasoning `{cwd['reasoning']['mean'] - base['reasoning']['mean']:+.1f}` tokens; "
            f"wall `{cwd['wall_ms']['mean'] - base['wall_ms']['mean']:+.1f}` ms; "
            f"first content `{cwd['first_content_ms']['mean'] - base['first_content_ms']['mean']:+.1f}` ms.",
            "",
        ])
    coding_base = summary["coding"]["go-base"]
    coding_cwd = summary["coding"]["go-cwd"]
    text_base = summary["text"]["go-base"]
    text_cwd = summary["text"]["go-cwd"]

    def change(after: float, before: float) -> float:
        return ((after - before) / before * 100) if before else 0.0

    gates_pass = all(
        summary[task][group]["successes"] == summary[task][group]["runs"]
        for task in ("coding", "text")
        for group in ("go-base", "go-cwd", "pi")
    )
    lines.extend([
        "## Observed decision", "",
        f"- All task/cell runs passed: `{'yes' if gates_pass else 'no'}`.",
        f"- Coding with cwd changed reasoning by `{change(coding_cwd['reasoning']['mean'], coding_base['reasoning']['mean']):+.1f}%` "
        f"and wall time by `{change(coding_cwd['wall_ms']['mean'], coding_base['wall_ms']['mean']):+.1f}%`.",
        f"- Plain text with cwd changed reasoning by `{change(text_cwd['reasoning']['mean'], text_base['reasoning']['mean']):+.1f}%` "
        f"and wall time by `{change(text_cwd['wall_ms']['mean'], text_base['wall_ms']['mean']):+.1f}%`.",
        f"- Recommendation: `{'enable cwd by default for workspace agents' if gates_pass else 'do not change the default yet'}`.",
        "",
    ])
    lines.extend([
        "## Interpretation gates", "",
        "- Coding success requires calculator.py changed, tests unchanged, and all unit tests passing.",
        "- Text success requires exactly five numbered items, all required concepts, no code fence, and at most 500 characters.",
        "- Compare Go base vs Go + cwd causally. Pi coding tools differ from Go tools, so Pi is an outcome reference rather than a pure transport benchmark.",
        "- Promote cwd to the default only if Go + cwd preserves task success on both tasks and does not introduce a material regression on plain text.",
        "", "## Initial wire checks", "", "```json", json.dumps(wire, ensure_ascii=False, indent=2), "```",
        "", "## Artifacts", "",
        "- `records.jsonl`: normalized run results and acceptance evidence.",
        "- `request-profile.json`: exact sampling and token-limit configuration used by the run.",
        "- `wire-requests.jsonl`: redacted wire requests.",
        "- `coding/<cell>/`: model logs and preserved modified fixture files.",
        "- `text/<cell>/`: model logs for the plain text task.",
    ])
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    (path.parent / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate cwd prompt behavior on coding and plain text tasks")
    parser.add_argument("--runs", type=int, default=3)
    parser.add_argument("--base-url", default=os.getenv("LLM_BASE_URL", common.DEFAULT_BASE_URL))
    parser.add_argument("--model", default=os.getenv("LLM_MODEL", common.DEFAULT_MODEL))
    parser.add_argument("--request-profile", default=str(DEFAULT_PROFILE))
    parser.add_argument("--output-dir", default="artifacts/comparison/cwd-generalization")
    parser.add_argument("--timeout", type=float, default=240.0)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs 必须 >= 1")
    pi_path = shutil.which("pi")
    if not pi_path:
        parser.error("未找到 pi CLI")
    try:
        profile = common.load_request_profile(args.request_profile)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        parser.error(f"request profile 无效: {error}")

    output_dir = (ROOT / args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    (output_dir / "request-profile.json").write_text(
        json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    records_path = output_dir / "records.jsonl"
    wire_path = output_dir / "wire-requests.jsonl"
    for path in (records_path, wire_path):
        if path.exists():
            path.unlink()
    env = os.environ.copy()
    env["GOCACHE"] = "/tmp/pi-golang-cwd-generalization-gocache"
    common.apply_go_request_profile(env, profile)
    records: List[Dict[str, Any]] = []

    with tempfile.TemporaryDirectory(prefix="pi-golang-cwd-generalization-") as temp_dir:
        temp_root = Path(temp_dir)
        binary = temp_root / "pi-agent"
        proxy_binary = temp_root / "llm-audit-proxy"
        common.build_go_binary(binary, env)
        common.build_go_binary(proxy_binary, env, "./cmd/llm-audit-proxy")
        parsed_url = urllib.parse.urlsplit(args.base_url)
        upstream = urllib.parse.urlunsplit((parsed_url.scheme, parsed_url.netloc, "", "", ""))
        api_path = parsed_url.path.rstrip("/") or "/v1"
        proxy, proxy_base_url, proxy_log = common.start_audit_proxy(
            proxy_binary, upstream, wire_path, output_dir / "audit-proxy.log"
        )
        pi_envs = {
            task: write_pi_config(temp_root / f"pi-{task}", f"{proxy_base_url}/pi-{task}{api_path}", args.model, profile)
            for task in ("coding", "text")
        }
        workspace = output_dir / "fixture-workspace"
        try:
            for run in range(1, args.runs + 1):
                task_order = ("coding", "text") if run % 2 else ("text", "coding")
                group_order = list(GROUPS[(run - 1) % len(GROUPS):] + GROUPS[:(run - 1) % len(GROUPS)])
                for task in task_order:
                    for group in group_order:
                        print(f"[{run}/{args.runs}] {task}/{group['id']}", flush=True)
                        initial_hashes = reset_fixture(workspace)
                        if group["backend"] == "pi-golang":
                            record, answer = run_go(binary, task, group, run, workspace, output_dir, env, proxy_base_url, api_path, args)
                        else:
                            record, answer = run_pi(pi_path, task, run, workspace, output_dir, pi_envs[task], args)
                        if task == "coding":
                            validation = validate_coding(workspace, initial_hashes, env)
                            preserved = output_dir / "coding" / group["id"] / f"run-{run:03d}-workspace"
                            if preserved.exists():
                                shutil.rmtree(preserved)
                            shutil.copytree(workspace, preserved)
                        else:
                            validation = validate_text(answer)
                            validation["artifact_sha256"] = sha256_text(answer) if answer else ""
                        finish_record(record, validation)
                        records.append(record)
                        with records_path.open("a", encoding="utf-8") as stream:
                            stream.write(json.dumps(record, ensure_ascii=False) + "\n")
        finally:
            proxy.terminate()
            try:
                proxy.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proxy.kill()
            proxy_log.close()
            if workspace.exists():
                shutil.rmtree(workspace)

    wire = initial_wire_summary(wire_path)
    (output_dir / "wire-validation.json").write_text(json.dumps(wire, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    report = output_dir / "report.md"
    write_report(report, args, records, wire)
    print(f"report: {report}")
    return 0 if all(record["success"] for record in records) else 1


if __name__ == "__main__":
    raise SystemExit(main())
