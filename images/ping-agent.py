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


def call(host, port, request, read_timeout=60):
    with socket.create_connection((host, port), timeout=10) as conn:
        conn.settimeout(read_timeout)
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
        # Client only: `docker version` also waits for the daemon, which is still starting under TCG; "docker daemon" checks that.
        "docker client": ("docker --version", lambda out: out.strip().startswith("Docker version")),
        "user in docker group": ("id -nG user", lambda out: "docker" in out.split()),
        # DNS must come from the DHCP-offered server (QEMU user networking: 10.0.2.3),
        # not a hard-coded resolver. The lease can land just after the agent starts.
        "dhcp dns server": (
            "for i in $(seq 30); do resolvectl dns 2>/dev/null | grep -qF 10.0.2.3 && break; sleep 1; done; "
            "resolvectl dns; readlink /etc/resolv.conf; grep -q '^nameserver 127.0.0.53' /etc/resolv.conf",
            lambda out: "10.0.2.3" in out,
        ),
        "resolves a name": (
            "for i in $(seq 15); do getent hosts github.com && exit 0; sleep 2; done; exit 1",
            lambda out: out.strip() != "",
        ),
        # sshd must be up and no ssh unit may be failed (a failed ssh.service
        # shows as "[FAILED] Failed to start ssh.service" on every boot). Prints
        # the unit state and journal on failure so CI shows the reason.
        "sshd healthy": (
            "for i in $(seq 60); do systemctl is-active -q ssh.service && break; sleep 1; done; "
            "bad=$(systemctl --failed --plain --no-legend | grep -E '^ssh'); "
            "if systemctl is-active -q ssh.service && [ -z \"$bad\" ]; then exit 0; fi; "
            "systemctl --failed --plain --no-pager; systemctl status ssh.service ssh.socket --no-pager; "
            "journalctl -u ssh.service -u ssh.socket -b --no-pager | tail -40; exit 1",
            lambda out: True,
        ),
        # The daemon may still be starting when the agent first answers.
        "docker daemon": (
            "for i in $(seq 240); do systemctl is-active -q docker && break; sleep 1; done; "
            "docker version --format '{{.Server.Version}}'",
            lambda out: out.strip() != "",
        ),
    })
    failed = False
    for label, (script, ok) in checks.items():
        # The daemon check waits longer: dockerd starts slowly under TCG emulation.
        secs = 270 if label == "docker daemon" else 55
        res = call(host, port, {"token": token, "op": "exec", "script": script, "timeoutSeconds": secs}, secs + 10)
        good = res.get("ok") and res.get("exitCode") == 0 and ok(res.get("stdout", ""))
        print("ping-agent: %s: %s" % (label, "ok" if good else "FAIL " + json.dumps(res)))
        failed = failed or not good
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
