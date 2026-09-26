#!/usr/bin/env python3
"""Run a 2x2 request-shape experiment against Pi on one local model.

Factors:
  1. system prompt includes Pi's <cwd> section
  2. user content uses [{"type":"text","text":"..."}] instead of a string

The script alternates all four Go variants and a Pi reference, records redacted
wire payloads, and reports reasoning, latency, visible output and determinism.
Only the Python standard library is required.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Sequence

import benchmark_compare as common


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_PROFILE = ROOT / "scripts" / "request-profile.lmstudio-seed42.json"

VARIANTS = (
    {"id": "base-text", "label": "Base", "cwd": False, "content": "text"},
    {"id": "cwd-text", "label": "CWD", "cwd": True, "content": "text"},
    {"id": "base-parts", "label": "Parts", "cwd": False, "content": "parts"},
    {"id": "cwd-parts", "label": "CWD + Parts", "cwd": True, "content": "parts"},
)


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def read_json_lines(path: Path) -> Iterable[Dict[str, Any]]:
    if not path.exists():
        return []
    return common.safe_json_lines(path.read_text(encoding="utf-8"))


def pi_final_text(stdout: str) -> str:
    final = ""
    for event in common.safe_json_lines(stdout):
        if event.get("type") != "message_end":
            continue
        message = event.get("message")
        if isinstance(message, dict) and message.get("role") == "assistant":
            final = common.assistant_text(message)
    return final


def summarize_group(records: Sequence[Dict[str, Any]], group: str) -> Dict[str, Any]:
    selected = [record for record in records if record.get("group") == group]
    successful = [record for record in selected if record.get("success")]

    def metric(name: str) -> Dict[str, float]:
        return common.stats([float(record.get(name, 0)) for record in successful])

    hashes = {record.get("artifact_sha256") for record in successful if record.get("artifact_sha256")}
    return {
        "runs": len(selected),
        "successes": len(successful),
        "success_rate": len(successful) / len(selected) if selected else 0,
        "wall_ms": metric("wall_ms"),
        "input": metric("input"),
        "output": metric("output"),
        "reasoning": metric("reasoning"),
        "visible_output": metric("visible_output"),
        "first_content_ms": metric("first_content_ms"),
        "unique_artifacts": len(hashes),
        "artifact_hashes": sorted(hashes),
    }


def mean(summary: Dict[str, Any], field: str) -> float:
    return float(summary[field]["mean"])


def factorial_effects(summary: Dict[str, Dict[str, Any]], field: str) -> Dict[str, float]:
    base = mean(summary["base-text"], field)
    cwd = mean(summary["cwd-text"], field)
    parts = mean(summary["base-parts"], field)
    both = mean(summary["cwd-parts"], field)
    return {
        "cwd_main_effect": round(((cwd - base) + (both - parts)) / 2, 3),
        "parts_main_effect": round(((parts - base) + (both - cwd)) / 2, 3),
        "interaction": round(both - cwd - parts + base, 3),
    }


def wire_validation(path: Path) -> Dict[str, Any]:
    records = list(read_json_lines(path))
    result: Dict[str, Any] = {}
    generation_keys = (
        "stream", "stream_options", "temperature", "max_tokens",
        "max_completion_tokens", "top_p", "seed", "store",
        "reasoning_effort", "thinking", "chat_template_kwargs",
    )
    generation_shapes = set()
    for runner in [variant["id"] for variant in VARIANTS] + ["pi"]:
        wire_runner = "pi" if runner == "pi" else "go-" + runner
        selected = [record for record in records if record.get("runner") == wire_runner]
        payload_hashes = {record.get("canonical_sha256") for record in selected}
        system_bytes = set()
        user_content_kinds = set()
        for record in selected:
            semantic = record.get("semantic") or {}
            generation = {key: semantic.get(key) for key in generation_keys if key in semantic}
            generation_shapes.add(json.dumps(generation, sort_keys=True, ensure_ascii=False))
            messages = semantic.get("messages") or []
            if len(messages) >= 2:
                system = messages[0].get("content") if isinstance(messages[0], dict) else None
                if isinstance(system, dict) and isinstance(system.get("bytes"), int):
                    system_bytes.add(system["bytes"])
                user = messages[1].get("content") if isinstance(messages[1], dict) else None
                user_content_kinds.add("parts" if isinstance(user, list) else "text")
        result[runner] = {
            "requests": len(selected),
            "unique_payloads": len(payload_hashes),
            "system_bytes": sorted(system_bytes),
            "user_content_kinds": sorted(user_content_kinds),
        }
    result["generation_fields_equal"] = len(generation_shapes) == 1
    return result


def closest_variant(summary: Dict[str, Dict[str, Any]]) -> str:
    pi_reasoning = max(mean(summary["pi"], "reasoning"), 1)
    pi_wall = max(mean(summary["pi"], "wall_ms"), 1)
    return min(
        (variant["id"] for variant in VARIANTS),
        key=lambda group: abs(mean(summary[group], "reasoning") - pi_reasoning) / pi_reasoning
        + abs(mean(summary[group], "wall_ms") - pi_wall) / pi_wall,
    )


def write_report(
    path: Path,
    args: argparse.Namespace,
    records: Sequence[Dict[str, Any]],
    wire: Dict[str, Any],
) -> None:
    groups = [variant["id"] for variant in VARIANTS] + ["pi"]
    summary = {group: summarize_group(records, group) for group in groups}
    labels = {variant["id"]: variant["label"] for variant in VARIANTS}
    labels["pi"] = "Pi reference"
    lines = [
        "# Go request-shape 2×2 validation",
        "",
        f"- Generated: {datetime.now(timezone.utc).isoformat()}",
        f"- Endpoint: `{args.base_url}`",
        f"- Model: `{args.model}`",
        f"- Runs per cell: `{args.runs}`; execution order rotates by round.",
        f"- Shared profile: `{args.request_profile}`",
        f"- Generation fields equal on wire: `{wire.get('generation_fields_equal')}`",
        "",
        "## Results",
        "",
        "Output tokens include reasoning. Visible output is calculated as output minus reasoning.",
        "",
        "| Cell | cwd | user content | success | wall ms | reasoning | visible output | first content ms | input | unique HTML |",
        "|---|---:|---|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for variant in VARIANTS:
        group = variant["id"]
        item = summary[group]
        lines.append(
            f"| {variant['label']} | {'yes' if variant['cwd'] else 'no'} | {variant['content']} | "
            f"{item['successes']}/{item['runs']} | {mean(item, 'wall_ms'):.1f} | {mean(item, 'reasoning'):.1f} | "
            f"{mean(item, 'visible_output'):.1f} | {mean(item, 'first_content_ms'):.1f} | "
            f"{mean(item, 'input'):.1f} | {item['unique_artifacts']} |"
        )
    pi = summary["pi"]
    lines.append(
        f"| Pi reference | yes | parts | {pi['successes']}/{pi['runs']} | {mean(pi, 'wall_ms'):.1f} | "
        f"{mean(pi, 'reasoning'):.1f} | {mean(pi, 'visible_output'):.1f} | n/a | {mean(pi, 'input'):.1f} | "
        f"{pi['unique_artifacts']} |"
    )
    effects = {
        "reasoning": factorial_effects(summary, "reasoning"),
        "wall_ms": factorial_effects(summary, "wall_ms"),
        "first_content_ms": factorial_effects(summary, "first_content_ms"),
    }
    winner = closest_variant(summary)
    lines.extend([
        "",
        "## Factor effects",
        "",
        "Effects are measured in the original metric units. Negative values mean the factor reduced the metric.",
        "",
        "```json",
        json.dumps(effects, ensure_ascii=False, indent=2),
        "```",
        "",
        "## Decision",
        "",
        f"- Closest Go cell to Pi by normalized reasoning and wall time: `{labels[winner]}`.",
        "- Prefer the smallest effective change: if Parts alone matches Pi, keep cwd out of the product prompt; "
        "if only CWD + Parts matches, enable both for the LM Studio compatibility profile.",
        "- Treat a cell as deterministic only when `unique HTML = 1` and token standard deviation is zero or negligible.",
        "- Do not attribute an effect unless the wire validation below shows identical generation fields and the intended two factors only.",
        "",
        "## Wire validation",
        "",
        "```json",
        json.dumps(wire, ensure_ascii=False, indent=2),
        "```",
        "",
        "## Artifacts",
        "",
        "- `records.jsonl`: one normalized result per run.",
        "- `wire-requests.jsonl`: redacted request shapes and hashes.",
        "- `wire-validation.json`: payload invariants for every cell.",
        "- `go/<cell>/`: Go audit logs and generated HTML.",
        "- `pi/`: Pi JSON event streams.",
    ])
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    (path.parent / "summary.json").write_text(
        json.dumps({"groups": summary, "effects": effects, "closest_to_pi": winner}, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def run_go(
    binary: Path,
    variant: Dict[str, Any],
    run: int,
    output_dir: Path,
    base_env: Dict[str, str],
    proxy_base_url: str,
    args: argparse.Namespace,
) -> Dict[str, Any]:
    run_dir = output_dir / "go" / variant["id"]
    run_dir.mkdir(parents=True, exist_ok=True)
    audit_path = run_dir / f"run-{run:03d}.jsonl"
    html_path = run_dir / f"run-{run:03d}.html"
    env = dict(base_env)
    env["AGENT_INCLUDE_WORKING_DIRECTORY"] = "true" if variant["cwd"] else "false"
    env["LLM_USER_CONTENT_FORMAT"] = variant["content"]
    command = [
        str(binary), "run", "--provider", "lmstudio",
        "--base-url", f"{proxy_base_url}/go-{variant['id']}{args.api_path}",
        "--model", args.model, "--prompt", common.USER_PROMPT,
        "--audit-file", str(audit_path), "--output", str(html_path),
        "--no-tools",
    ]
    code, stdout, stderr, wall_ms = common.run_command(command, env, args.timeout)
    parsed = common.parse_go_audit(audit_path)
    parsed.update(common.parse_go_stdout(stdout))
    parsed.update(common.validate_html_artifact(html_path))
    artifact = html_path.read_text(encoding="utf-8") if html_path.exists() else ""
    parsed.update({
        "backend": "pi-golang", "group": variant["id"], "run": run,
        "cwd": variant["cwd"], "user_content_format": variant["content"],
        "wall_ms": round(wall_ms, 3), "exit_code": code,
        "stderr_tail": stderr[-1000:], "artifact_sha256": sha256_text(artifact) if artifact else "",
    })
    common.flatten_usage(parsed)
    parsed["visible_output"] = max(parsed["output"] - parsed["reasoning"], 0)
    parsed["success"] = bool(parsed.get("transport_success")) and bool(parsed.get("task_success")) and code == 0
    return parsed


def run_pi(
    pi_path: str,
    run: int,
    output_dir: Path,
    pi_env: Dict[str, str],
    args: argparse.Namespace,
) -> Dict[str, Any]:
    raw_path = output_dir / "pi" / f"run-{run:03d}.jsonl"
    command = [
        pi_path, "--provider", "lmstudio", "--model", args.model,
        "--api-key", "lm-studio-local", "--no-session", "--no-tools",
        "--no-context-files", "--no-skills", "--no-prompt-templates",
        "--no-extensions", "--offline", "--thinking", "off",
        "--system-prompt", common.load_text(ROOT / "internal/prompt/base.md"),
        "--mode", "json", "--print", "--", common.USER_PROMPT,
    ]
    code, stdout, stderr, wall_ms = common.run_command(command, pi_env, args.timeout)
    raw_path.write_text(stdout, encoding="utf-8")
    parsed = common.parse_pi_events(stdout)
    artifact = pi_final_text(stdout)
    parsed.update({
        "backend": "pi", "group": "pi", "run": run,
        "cwd": True, "user_content_format": "parts",
        "wall_ms": round(wall_ms, 3), "exit_code": code,
        "stderr_tail": stderr[-1000:], "artifact_sha256": sha256_text(artifact) if artifact else "",
        "first_content_ms": 0,
    })
    common.flatten_usage(parsed)
    parsed["visible_output"] = max(parsed["output"] - parsed["reasoning"], 0)
    parsed["success"] = bool(parsed.get("transport_success")) and bool(parsed.get("task_success")) and code == 0
    return parsed


def main() -> int:
    parser = argparse.ArgumentParser(description="Run Go/Pi request-shape 2x2 validation")
    parser.add_argument("--runs", type=int, default=3, help="每个 2x2 cell 和 Pi 的运行次数，默认 3")
    parser.add_argument("--base-url", default=os.getenv("LLM_BASE_URL", common.DEFAULT_BASE_URL))
    parser.add_argument("--model", default=os.getenv("LLM_MODEL", common.DEFAULT_MODEL))
    parser.add_argument("--request-profile", default=str(DEFAULT_PROFILE))
    parser.add_argument("--output-dir", default="artifacts/comparison/request-shape-2x2")
    parser.add_argument("--timeout", type=float, default=180.0)
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
    (output_dir / "pi").mkdir(exist_ok=True)
    records_path = output_dir / "records.jsonl"
    wire_path = output_dir / "wire-requests.jsonl"
    for path in (records_path, wire_path):
        if path.exists():
            path.unlink()
    (output_dir / "request-profile.json").write_text(
        json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )

    env = os.environ.copy()
    env["LLM_MODEL"] = args.model
    env["GOCACHE"] = "/tmp/pi-golang-request-shape-gocache"
    common.apply_go_request_profile(env, profile)
    records: List[Dict[str, Any]] = []

    with tempfile.TemporaryDirectory(prefix="pi-golang-request-shape-") as temp_dir:
        temp_root = Path(temp_dir)
        binary = temp_root / "pi-agent"
        proxy_binary = temp_root / "llm-audit-proxy"
        common.build_go_binary(binary, env)
        common.build_go_binary(proxy_binary, env, "./cmd/llm-audit-proxy")
        parsed_base_url = urllib.parse.urlsplit(args.base_url)
        upstream = urllib.parse.urlunsplit((parsed_base_url.scheme, parsed_base_url.netloc, "", "", ""))
        args.api_path = parsed_base_url.path.rstrip("/") or "/v1"
        proxy, proxy_base_url, proxy_log = common.start_audit_proxy(
            proxy_binary, upstream, wire_path, output_dir / "audit-proxy.log"
        )
        pi_agent_dir = temp_root / "pi-agent-dir"
        pi_agent_dir.mkdir()
        common.write_pi_models_config(pi_agent_dir, f"{proxy_base_url}/pi{args.api_path}", args.model, profile)
        pi_env = dict(env)
        pi_env["PI_CODING_AGENT_DIR"] = str(pi_agent_dir)
        pi_env["PI_OFFLINE"] = "1"
        try:
            cells = list(VARIANTS) + [{"id": "pi"}]
            for run in range(1, args.runs + 1):
                rotated = cells[(run - 1) % len(cells):] + cells[:(run - 1) % len(cells)]
                for cell in rotated:
                    print(f"[{run}/{args.runs}] {cell['id']}", flush=True)
                    if cell["id"] == "pi":
                        record = run_pi(pi_path, run, output_dir, pi_env, args)
                    else:
                        record = run_go(binary, cell, run, output_dir, env, proxy_base_url, args)
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

    wire = wire_validation(wire_path)
    (output_dir / "wire-validation.json").write_text(
        json.dumps(wire, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    report = output_dir / "report.md"
    write_report(report, args, records, wire)
    print(f"report: {report}")
    return 0 if all(record.get("success") for record in records) and wire.get("generation_fields_equal") else 1


if __name__ == "__main__":
    raise SystemExit(main())
