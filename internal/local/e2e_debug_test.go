//go:build e2e

package local

import (
	"context"
	"os/exec"
	"strings"
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
	sh := func(name string, args ...string) string {
		out, _ := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Logf("docker: %s", sh("docker", "--version"))
	// Probes: passwd -S root only works with euid 0; sudo -n id -u prints 0 when setuid works.
	script := `echo "sudo: $(sudo -n id -u 2>&1 | head -1)"; echo "passwd: $(passwd -S root 2>&1 | head -1)"; echo "mount: $(mount --bind /tmp /tmp 2>&1 | head -1)"; grep -E "NoNewPrivs" /proc/self/status`
	variants := []struct {
		name string
		args []string
	}{
		{"runc control, user 1001", []string{"--runtime=runc", "--user", "1001:1001"}},
		{"runsc user 1001", []string{"--runtime=runsc", "--user", "1001:1001"}},
		{"runsc user name 'user'", []string{"--runtime=runsc", "--user", "user"}},
		{"runsc, no-new-privileges=false", []string{"--runtime=runsc", "--user", "1001:1001", "--security-opt", "no-new-privileges=false"}},
		{"runsc, read-only rootfs", []string{"--runtime=runsc", "--user", "1001:1001", "--read-only"}},
		{"runsc, cap-add ALL", []string{"--runtime=runsc", "--user", "1001:1001", "--cap-add=ALL"}},
		{"runsc, annotation overlay2=none", []string{"--runtime=runsc", "--user", "1001:1001", "--annotation", "dev.gvisor.flag.overlay2=none"}},
		{"runsc, host network", []string{"--runtime=runsc", "--user", "1001:1001", "--network=host"}},
	}
	for _, v := range variants {
		args := append(append([]string{"run", "--rm", "--entrypoint", "sh"}, v.args...), tag, "-c", script)
		t.Logf("=== %s\n%s", v.name, sh("docker", args...))
	}
	// Stock image, no setuid binary of ours: does passwd work as euid 0?
	t.Logf("=== stock ubuntu:noble runsc user 1001\n%s", sh("docker", "run", "--rm", "--runtime=runsc", "--user", "1001:1001", "ubuntu:noble", "sh", "-c", `echo "passwd: $(passwd -S root 2>&1 | head -1)"; ls -l /usr/bin/passwd`))
	// A setuid check from a process that has just exec'd as the unprivileged user via su from root.
	t.Logf("=== root su to user\n%s", sh("docker", "run", "--rm", "--runtime=runsc", "--entrypoint", "sh", tag, "-c", `su user -c 'sudo -n id -u 2>&1 | head -1'`))
}
