#!/usr/bin/env python3
"""黑盒集成测试：mock LLM -> Go CLI -> 完整 JSONL 审计记录。"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

from mock_openai import start_server


ROOT = Path(__file__).resolve().parents[2]


def main() -> int:
    server, thread = start_server()
    port = server.server_address[1]
    try:
        with tempfile.TemporaryDirectory(prefix="pi-golang-integration-") as directory:
            audit_file = Path(directory) / "llm.jsonl"
            env = os.environ.copy()
            env.update(
                {
                    "LLM_PROVIDER": "custom",
                    "LLM_BASE_URL": f"http://127.0.0.1:{port}/v1",
                    "LLM_MODEL": "integration-mock",
                    "AUDIT_LOG_FILE": str(audit_file),
                    "AUDIT_CONTENT_MODE": "full",
                }
            )
            result = subprocess.run(
                ["go", "run", ".", "run", "--prompt", "integration input"],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                timeout=90,
                check=False,
            )
            if result.returncode != 0:
                raise AssertionError(f"Go CLI failed\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}")
            if "mock answer: integration input" not in result.stdout:
                raise AssertionError(f"unexpected CLI output:\n{result.stdout}")

            records = [json.loads(line) for line in audit_file.read_text(encoding="utf-8").splitlines()]
            phases = [record["phase"] for record in records]
            if phases != ["request", "response"]:
                raise AssertionError(f"unexpected audit phases: {phases}")
            if records[0]["request"]["Messages"][-1]["content"] != "integration input":
                raise AssertionError(f"request audit missing user input: {records[0]}")
            if records[1]["response"]["Content"] != "mock answer: integration input":
                raise AssertionError(f"response audit missing model output: {records[1]}")
            if records[0]["run_id"] != records[1]["run_id"]:
                raise AssertionError("audit events do not share a run_id")

            interactive = subprocess.run(
                ["go", "run", ".", "run", "--interactive"],
                cwd=ROOT,
                env=env,
                input="/help\nfirst turn\nsecond turn\n/exit\n",
                text=True,
                capture_output=True,
                timeout=90,
                check=False,
            )
            if interactive.returncode != 0:
                raise AssertionError(
                    "Interactive Go CLI failed\n"
                    f"stdout:\n{interactive.stdout}\n"
                    f"stderr:\n{interactive.stderr}"
                )
            for answer in ("mock answer: first turn", "mock answer: second turn"):
                if answer not in interactive.stdout:
                    raise AssertionError(f"interactive output missing {answer!r}: {interactive.stdout}")
            if "当前对话已清空" in interactive.stdout:
                raise AssertionError("interactive test unexpectedly reset the conversation")

            records = [json.loads(line) for line in audit_file.read_text(encoding="utf-8").splitlines()]
            if len(records) != 6:
                raise AssertionError(f"interactive run should add four audit records: {records}")
            second_request = records[4]["request"]["Messages"]
            if [message["content"] for message in second_request if message["role"] == "user"][-2:] != [
                "first turn",
                "second turn",
            ]:
                raise AssertionError(f"interactive history missing previous turn: {second_request}")
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)

    print("integration test passed")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (AssertionError, OSError, subprocess.TimeoutExpired) as error:
        print(f"integration test failed: {error}", file=sys.stderr)
        raise SystemExit(1)
