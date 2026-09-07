#!/opt/homebrew/bin/python3
"""Verify the Zed Codex worker with real file tools; consumes Zed credit."""
import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import tempfile
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--base-url", default="http://127.0.0.1:8317/v1")
args = parser.parse_args()
os.umask(0o077)
state = Path("/Users/maixu/.local/state/cliproxyapi/verification")
state.mkdir(parents=True, exist_ok=True)
log_path = state / ("worker-" + str(time.time_ns()) + ".jsonl")
# A failed or interrupted attempt must not leave an earlier success current.
(state / "worker-latest.json").write_text(json.dumps({
    "success": False, "status": "not_verified", "log": str(log_path),
}) + "\n")
with tempfile.TemporaryDirectory(prefix="zed-sol-worker-") as temp:
    work = Path(temp)
    numbers = [secrets.randbelow(10000) for _ in range(5)]
    (work / "numbers.json").write_text(json.dumps(numbers) + "\n")
    final_path = work / "final.txt"
    prompt = (
        "Work only in the current directory. Read numbers.json with a tool. "
        "Use apply_patch to write result.json with exactly count and sum, computed from that file. "
        "Run a Python assertion that independently compares result.json with numbers.json. "
        "Do not install software or use the network. Reply exactly ZED_WORKER_OK only after verification."
    )
    command = [
        "/Users/maixu/.local/bin/codex-zed", "exec", "--ephemeral",
        "--skip-git-repo-check", "-C", str(work),
        "-c", "model_providers.zed-gateway.base_url=" + json.dumps(args.base_url),
        "--json", "-o", str(final_path), prompt,
    ]
    with log_path.open("w") as log:
        child = subprocess.Popen(command, stdout=log, stderr=log, start_new_session=True)
        try:
            code = child.wait(timeout=150)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGTERM)
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait()
            raise SystemExit("Worker timed out; private log: " + str(log_path))
    events = []
    for line in log_path.read_text(errors="replace").splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            pass
    if code:
        errors = [e for e in events if e.get("type") in ("error", "turn.failed")]
        print(json.dumps({"exit": code, "errors": errors, "log": str(log_path)}))
        raise SystemExit(code)
    result = json.loads((work / "result.json").read_text())
    assert result == {"count": len(numbers), "sum": sum(numbers)}, result
    assert final_path.read_text().strip() == "ZED_WORKER_OK"
    completed = [e.get("item", {}) for e in events if e.get("type") == "item.completed"]
    assert any(i.get("type") == "file_change" for i in completed), "No actual patch event"
    assert any(i.get("type") == "command_execution" for i in completed), "No command execution"
    usage = next((e.get("usage") for e in events if e.get("type") == "turn.completed"), None)
    summary = {"success": True, "model": "zed/gpt-5.6-sol", "result": result, "usage": usage, "log": str(log_path)}
    (state / "worker-latest.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary))
