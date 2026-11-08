#!/usr/bin/env python3
"""Smoke tests for instabox-keepalive against mock/mock_server.py.

Runs the real binary with --once against a scripted mock and asserts which
API calls it made. Usage:  python3 mock/run_tests.py   (from the project dir,
after `go build -o keepalive .`)
"""
import json
import os
import subprocess
import sys
import time
import urllib.request

BIN = os.environ.get("BIN", "./keepalive")
PORT = int(os.environ.get("MOCK_PORT", "18089"))
BASE = f"http://127.0.0.1:{PORT}"
MID = "mch_test"


def calls():
    with urllib.request.urlopen(f"{BASE}/__calls") as r:
        return [c["method"] + " " + c["path"].split("?")[0] for c in json.load(r)["calls"]]


def idx(cs, needle):
    for i, c in enumerate(cs):
        if needle in c:
            return i
    return -1


def has(cs, needle):
    return idx(cs, needle) >= 0


def scenario(name, mock_env, args, expect):
    env = {**os.environ, "INSTABOX_API_KEY": "ibk_test", "MOCK_PORT": str(PORT), **mock_env}
    mock = subprocess.Popen([sys.executable, "mock/mock_server.py"], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    time.sleep(0.7)
    try:
        run = subprocess.run([BIN, "--api-base", BASE, "--machine", "test", "--once", "-verbose", *args],
                             env=env, capture_output=True, text=True, timeout=90)
        cs = calls()
        problems = expect(cs, run)
        if run.returncode != 0:
            problems.append(f"exit code {run.returncode}\n--- stdout ---\n{run.stdout}\n--- stderr ---\n{run.stderr}")
        if problems:
            print(f"FAIL  {name}")
            for p in problems:
                print(f"      - {p}")
            print(f"      calls: {cs}")
            return False
        print(f"PASS  {name}")
        return True
    finally:
        mock.terminate()
        mock.wait(timeout=5)


def main():
    ok = []

    ok.append(scenario(
        "running machine -> lease renewed, no restart",
        {"MOCK_STATE": "running", "MOCK_RUN_AGE_MIN": "60", "MOCK_MAX_RUN": "480"},
        [],
        lambda cs, r: ([] if has(cs, f"PUT /v1/machines/{MID}/activity-leases/keepalive") else ["no lease PUT"])
        + (["unexpected suspend"] if has(cs, f"POST /v1/machines/{MID}/suspend") else [])
        + (["unexpected resume"] if has(cs, f"POST /v1/machines/{MID}/resume") else []),
    ))

    ok.append(scenario(
        "run age 6h20m -> proactive suspend+resume before 8h cap",
        {"MOCK_STATE": "running", "MOCK_RUN_AGE_MIN": "380", "MOCK_MAX_RUN": "480"},
        [],
        lambda cs, r: ([] if has(cs, f"POST /v1/machines/{MID}/suspend") else ["no suspend"])
        + ([] if has(cs, f"POST /v1/machines/{MID}/resume") else ["no resume"])
        + ([] if idx(cs, f"POST /v1/machines/{MID}/suspend") < idx(cs, f"POST /v1/machines/{MID}/resume") else ["resume before suspend"])
        + ([] if has(cs, f"PUT /v1/machines/{MID}/activity-leases/keepalive") else ["lease not re-taken after restart"]),
    ))

    ok.append(scenario(
        "dynamic window (120m) -> restart fires at deadline-margin (10m)",
        {"MOCK_STATE": "running", "MOCK_RUN_AGE_MIN": "115", "MOCK_MAX_RUN": "120"},
        [],
        lambda cs, r: ([] if has(cs, f"POST /v1/machines/{MID}/suspend") else ["no suspend (deadline-aware restart missed)"])
        + ([] if has(cs, f"POST /v1/machines/{MID}/resume") else ["no resume"]),
    ))

    ok.append(scenario(
        "self-paused machine (autoSuspendedAt) -> wake",
        {"MOCK_STATE": "suspended", "MOCK_AUTO_SUSPENDED": "1"},
        [],
        lambda cs, r: ([] if has(cs, f"POST /v1/machines/{MID}/wake") else ["no wake"])
        + (["unexpected start"] if has(cs, f"POST /v1/machines/{MID}/start") else []),
    ))

    ok.append(scenario(
        "machine in cooldown -> waits, touches nothing",
        {"MOCK_STATE": "suspended", "MOCK_COOLDOWN_SEC": "3600"},
        [],
        lambda cs, r: []
        + (["wake during cooldown"] if has(cs, f"POST /v1/machines/{MID}/wake") else [])
        + (["resume during cooldown"] if has(cs, f"POST /v1/machines/{MID}/resume") else [])
        + (["start during cooldown"] if has(cs, f"POST /v1/machines/{MID}/start") else []),
    ))

    ok.append(scenario(
        "stopped machine + -auto-start -> start",
        {"MOCK_STATE": "stopped"},
        ["-auto-start"],
        lambda cs, r: ([] if has(cs, f"POST /v1/machines/{MID}/start") else ["no start"]),
    ))

    print(f"\n{sum(ok)}/{len(ok)} scenarios passed")
    sys.exit(0 if all(ok) else 1)


if __name__ == "__main__":
    main()
