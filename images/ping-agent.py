#!/usr/bin/env python3
"""Waits for the Dock guest agent, then pings it and runs a few checks.

Usage: ping-agent.py HOST PORT TOKEN TIMEOUT_SECONDS MODE
MODE is fwcfg or cmdline: how the boot test passed the guest config.
Speaks the agent JSON-line protocol (see internal/host/fast.go).
"""
import base64
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
    host, port, token, timeout, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4]), sys.argv[5]
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
    checks = {}
    if mode == "fwcfg":
        # The token must not reach the kernel command line or the journal.
        # The script decodes it so the check itself does not carry it.
        enc = base64.b64encode(token.encode()).decode()
        checks["token not logged"] = (
            "t=$(echo %s | base64 -d); ! grep -qF \"$t\" /proc/cmdline && ! journalctl -b --no-pager | grep -qF \"$t\"" % enc,
            lambda out: True,
        )
    checks.update({
        "hostname": ("hostname", lambda out: out.strip() == "boot-test"),
        "root grown": ("df -BM --output=size / | tail -1", lambda out: int(out.strip().rstrip("M")) > 5000),
        "agent config mode": ("stat -c %a /etc/dock-fast-agent.json", lambda out: out.strip() == "600"),
        "docker client": ("docker version --format '{{.Client.Version}}'", lambda out: out.strip() != ""),
        "user in docker group": ("id -nG user", lambda out: "docker" in out.split()),
        # The daemon may still be starting when the agent first answers.
        "docker daemon": (
            "for i in $(seq 45); do systemctl is-active -q docker && break; sleep 1; done; "
            "docker version --format '{{.Server.Version}}'",
            lambda out: out.strip() != "",
        ),
    })
    failed = False
    for label, (script, ok) in checks.items():
        res = call(host, port, {"token": token, "op": "exec", "script": script, "timeoutSeconds": 55})
        good = res.get("ok") and res.get("exitCode") == 0 and ok(res.get("stdout", ""))
        print("ping-agent: %s: %s" % (label, "ok" if good else "FAIL " + json.dumps(res)))
        failed = failed or not good
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
