#!/usr/bin/env python3
"""Minimal InstaBox API mock for instabox-keepalive smoke tests.

Implements just the endpoints the daemon uses, with behaviour driven by env:
  MOCK_STATE         running|suspended|stopped        (default running)
  MOCK_RUN_AGE_MIN   minutes since runStartedAt        (default 60)
  MOCK_MAX_RUN       maxRunMinutes                     (default 480)
  MOCK_AUTO_SUSPENDED=1  sets autoSuspendedAt (self-pause -> wake path)
  MOCK_COOLDOWN_SEC  >0 sets cooldownUntil now+N       (daemon must wait)
  MOCK_MACHINE_ID    machine id                        (default mch_test)
  MOCK_MACHINE_NAME  machine name                      (default test)
  MOCK_KEY           required X-API-Key                (default ibk_test)
  MOCK_PORT          listen port                       (default 18080)

Every request is recorded; GET /__calls returns them for assertions.
"""
import json
import os
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CFG = {
    "state": os.environ.get("MOCK_STATE", "running"),
    "run_age_min": float(os.environ.get("MOCK_RUN_AGE_MIN", "60")),
    "max_run": int(os.environ.get("MOCK_MAX_RUN", "480")),
    "auto_suspended": os.environ.get("MOCK_AUTO_SUSPENDED", "") == "1",
    "cooldown_sec": int(os.environ.get("MOCK_COOLDOWN_SEC", "0")),
    "machine_id": os.environ.get("MOCK_MACHINE_ID", "mch_test"),
    "machine_name": os.environ.get("MOCK_MACHINE_NAME", "test"),
    "key": os.environ.get("MOCK_KEY", "ibk_test"),
}
CALLS = []
OPS = {}
OP_N = [0]


def now():
    return datetime.now(timezone.utc)


def iso(dt):
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


def machine():
    m = {
        "id": CFG["machine_id"],
        "name": CFG["machine_name"],
        "state": CFG["state"],
        "free": True,
        "runStartedAt": iso(now() - timedelta(minutes=CFG["run_age_min"])),
        "maxRunMinutes": CFG["max_run"],
        "cooldownUntil": None,
        "autoSuspendedAt": None,
        "operationId": None,
        "abuseLock": None,
        "cooldownReason": None,
    }
    if CFG["auto_suspended"] or CFG["state"] == "suspended":
        if CFG["auto_suspended"]:
            m["autoSuspendedAt"] = iso(now() - timedelta(minutes=5))
    if CFG["cooldown_sec"] > 0:
        m["cooldownUntil"] = iso(now() + timedelta(seconds=CFG["cooldown_sec"]))
    return m


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, obj=None, raw=None):
        body = raw if raw is not None else (json.dumps(obj).encode() if obj is not None else b"")
        self.send_response(code)
        if body:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def _err(self, code, err_code, message, retryable=False, retry_after=None):
        e = {"code": err_code, "message": message, "requestId": "req_mock",
             "retryable": retryable, "operationId": None}
        if retry_after is not None:
            e["retryAfterSeconds"] = retry_after
        self._send(code, {"error": e})

    def _op(self, typ):
        OP_N[0] += 1
        op = {"id": f"op_mock{OP_N[0]}", "state": "succeeded", "type": typ,
              "machineId": CFG["machine_id"], "trigger": "request",
              "runtime": {"processesPreserved": True, "ready": True}}
        OPS[op["id"]] = op
        return op

    def _route(self, method):
        path = self.path.split("?")[0]
        CALLS.append({"method": method, "path": self.path})

        if path == "/__calls":
            return self._send(200, {"calls": CALLS})

        if self.headers.get("X-API-Key") != CFG["key"]:
            return self._err(401, "invalid_key", "bad mock key")

        parts = [p for p in path.split("/") if p]
        # /v1/machines
        if parts == ["v1", "machines"] and method == "GET":
            return self._send(200, {"machines": [machine()], "nextCursor": None})
        # /v1/machines/{id}
        if len(parts) == 3 and parts[:2] == ["v1", "machines"]:
            mid = parts[2]
            if mid != CFG["machine_id"]:
                return self._err(404, "not_found", "no such machine")
            if method == "GET":
                return self._send(200, machine())
        # /v1/machines/{id}/activity-leases/{leaseID}
        if len(parts) == 5 and parts[2:] and parts[2] == CFG["machine_id"] and parts[3] == "activity-leases":
            if method == "PUT":
                ttl = 300
                n = int(self.headers.get("Content-Length", 0) or 0)
                if n:
                    ttl = json.loads(self.rfile.read(n)).get("ttlSeconds", 300)
                return self._send(200, {"id": parts[4], "machineId": parts[2],
                                        "expiresAt": iso(now() + timedelta(seconds=ttl))})
            if method == "DELETE":
                return self._send(204)
        # /v1/machines/{id}/{action}
        if len(parts) == 4 and parts[:2] == ["v1", "machines"] and parts[2] == CFG["machine_id"]:
            action = parts[3]
            if method == "POST" and action in ("wake", "suspend", "resume", "start", "stop"):
                if CFG["cooldown_sec"] > 0 and action in ("wake", "resume", "start"):
                    return self._err(409, "machine_cooldown", "machine is cooling down",
                                     retryable=True, retry_after=CFG["cooldown_sec"])
                if action == "wake" and CFG["state"] != "suspended":
                    return self._err(409, "operation_conflict", "machine is not idle-suspended")
                return self._send(202, self._op(action))
        # /v1/operations/{id}
        if len(parts) == 3 and parts[:2] == ["v1", "operations"]:
            op = OPS.get(parts[2])
            if not op:
                return self._err(404, "not_found", "no such operation")
            return self._send(200, op)

        return self._err(404, "not_found", f"mock has no route {method} {path}")

    def do_GET(self):
        self._route("GET")

    def do_POST(self):
        self._route("POST")

    def do_PUT(self):
        self._route("PUT")

    def do_DELETE(self):
        self._route("DELETE")

    def do_PATCH(self):
        self._route("PATCH")


if __name__ == "__main__":
    port = int(os.environ.get("MOCK_PORT", "18080"))
    print(f"mock listening on :{port} state={CFG['state']} run_age_min={CFG['run_age_min']}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
