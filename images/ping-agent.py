#!/usr/bin/env python3
"""Waits for the Dock guest agent, then pings it and runs a few checks.

Usage: ping-agent.py HOST PORT TOKEN TIMEOUT_SECONDS
Speaks the agent JSON-line protocol (see internal/host/fast.go).
"""
import json
import socket
import sys
import time


def call(host, port, request):
    with socket.create_connection((host, port), timeout=10) as conn:
        conn.settimeout(60)
        conn.sendall((json.dumps(request) + "\n").encode())
        return json.loads(conn.makefile("rb").readline().decode())


def main():
    host, port, token, timeout = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
    deadline = time.time() + timeout
    while True:
        try:
            if call(host, port, {"token": token, "op": "ping"}).get("ok"):
                break
        except (OSError, ValueError):
            pass
        if time.time() > deadline:
            print("ping-agent: timed out waiting for the agent", file=sys.stderr)
            return 1
        time.sleep(2)
    checks = {
        "hostname": ("hostname", lambda out: out.strip() == "boot-test"),
        "root grown": ("df -BM --output=size / | tail -1", lambda out: int(out.strip().rstrip("M")) > 3000),
        "agent config mode": ("stat -c %a /etc/dock-fast-agent.json", lambda out: out.strip() == "600"),
    }
    failed = False
    for label, (script, ok) in checks.items():
        res = call(host, port, {"token": token, "op": "exec", "script": script, "timeoutSeconds": 30})
        good = res.get("ok") and res.get("exitCode") == 0 and ok(res.get("stdout", ""))
        print("ping-agent: %s: %s" % (label, "ok" if good else "FAIL " + json.dumps(res)))
        failed = failed or not good
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
