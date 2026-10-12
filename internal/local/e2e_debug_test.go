//go:build e2e

package local

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TEMPORARY: diagnoses why setuid sudo fails under runsc.
func TestE2ELocalGVisorDockDebug(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	g := NewGVisor()
	tag, err := g.EnsureImage(ctx, func(s string) { t.Log(s) })
	if err != nil {
		t.Fatal(err)
	}
	sh := func(name string, args ...string) {
		out, _ := exec.CommandContext(ctx, name, args...).CombinedOutput()
		t.Logf("$ %s %v\n%s", name, args, out)
	}
	sh("runsc", "--version")
	sh("sh", "-c", "cat /etc/docker/daemon.json; docker info --format '{{json .Runtimes}}'")
	script := `echo "--- NoNewPrivs/caps"; grep -E "NoNewPrivs|^Cap|Seccomp" /proc/self/status
echo "--- root mount"; grep -E " / | /usr " /proc/self/mountinfo
echo "--- sudo file"; ls -l /usr/bin/sudo; stat -c '%a %u' /usr/bin/sudo
echo "--- sudo"; sudo -n id -u 2>&1
echo "--- su-like setuid test"; /usr/bin/passwd -S 2>&1 | head -2`
	base := []string{"run", "--rm", "--runtime=runsc", "--entrypoint", "sh"}
	variants := map[string][]string{
		"A baseline user 1001":            {"--user", "1001:1001"},
		"B seccomp unconfined":            {"--user", "1001:1001", "--security-opt", "seccomp=unconfined"},
		"C apparmor unconfined":           {"--user", "1001:1001", "--security-opt", "apparmor=unconfined"},
		"D cap-add setuid setgid":         {"--user", "1001:1001", "--cap-add=SETUID", "--cap-add=SETGID"},
		"E user 1001 + tmpfs /tmp + pids": {"--user", "1001:1001", "--tmpfs", "/tmp", "--pids-limit", "1024"},
		"F as root (no --user)":           {},
	}
	for name, extra := range variants {
		args := append(append(append([]string{}, base...), extra...), tag, "-c", script)
		t.Logf("=== %s", name)
		sh("docker", args...)
	}
	// An ubuntu image with the stock rootfs, to separate our image from the runtime.
	sh("docker", "run", "--rm", "--runtime=runsc", "--user", "1001:1001", "--entrypoint", "sh", tag, "-c", "grep -c . /proc/self/mountinfo; id")
	_ = os.Stderr
}
