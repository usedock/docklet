package local

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/usedock/docklet/internal/guestagent"
)

// Runtime names a local dock can have; an empty Record.Runtime means RuntimeQEMU.
const (
	RuntimeQEMU   = "qemu"
	RuntimeGVisor = "gvisor"
)

// The agent and the entrypoint are copies of host.FastAgentProgramText and
// scripts/dock-container-entrypoint.sh (go:embed cannot reach outside the package, and the
// CLI cannot import internal/host on every OS); gvisor_drift_test.go fails when they diverge.
var (
	//go:embed assets/dock-fast-agent.py
	gvAgentProgram string
	//go:embed assets/dock-container-entrypoint.sh
	gvEntrypoint string
)

// gvDockerfile is the local image: the fleet gVisor image without the Go codegen stage.
const gvDockerfile = `FROM ubuntu:noble
COPY dock-fast-agent.py /usr/local/libexec/dock-fast-agent.py
COPY dock-entrypoint /usr/local/bin/dock-entrypoint
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl git sudo python3; \
    chmod 0755 /usr/local/bin/dock-entrypoint /usr/local/libexec/dock-fast-agent.py; \
    useradd -u 1001 -m -s /bin/bash -G sudo user; \
    echo "user ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/user; \
    chmod 0440 /etc/sudoers.d/user; \
    mkdir -p /home/user/work /run/dock-fast; \
    chown user:user /home/user/work /run/dock-fast; \
    apt-get clean; \
    rm -rf /usr/share/doc /usr/share/man /var/cache/apt/archives /var/lib/apt/lists /tmp/* /var/tmp/* /root/.cache /var/log/*
WORKDIR /home/user/work
ENTRYPOINT ["/usr/local/bin/dock-entrypoint"]
`

// Container limits of a local gVisor dock.
const (
	gvUID       = 1001
	gvPidsLimit = 1024
	gvImageRepo = "dock-local-gvisor"
	// gvAgentConfigPath is where the entrypoint looks for the agent config.
	gvAgentConfigPath = "/etc/dock-fast-agent.json"
)

// GVisorInstallFix says how to get runsc registered with Docker.
const GVisorInstallFix = "Install gVisor (https://gvisor.dev/docs/user_guide/install/), then run: sudo runsc install && sudo systemctl restart docker"

// DockerRun runs the docker CLI with args and optional stdin.
type DockerRun func(ctx context.Context, stdin io.Reader, args ...string) (stdout, stderr string, err error)

func execDocker(ctx context.Context, stdin io.Reader, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// GVisor drives local docks that run as gVisor containers. Fields left nil use the real machine.
type GVisor struct {
	GOOS string
	// Run executes the docker CLI; nil uses os/exec.
	Run DockerRun
	// LookPath finds docker on PATH; nil uses exec.LookPath.
	LookPath func(string) (string, error)
}

// NewGVisor returns a driver for this machine.
func NewGVisor() *GVisor { return &GVisor{GOOS: runtime.GOOS} }

// GVisorPreflight checks that this machine can run gVisor docks (for dock doctor).
func GVisorPreflight(ctx context.Context) error { return NewGVisor().Preflight(ctx) }

func (g *GVisor) docker(ctx context.Context, stdin io.Reader, args ...string) (string, string, error) {
	if g.Run != nil {
		return g.Run(ctx, stdin, args...)
	}
	return execDocker(ctx, stdin, args...)
}

// Preflight fails with a coded, actionable error when gVisor docks cannot run here.
func (g *GVisor) Preflight(ctx context.Context) error {
	if g.GOOS != "linux" {
		return errf("unsupported_os", "--runtime gvisor is only supported on Linux, not %s", g.GOOS)
	}
	look := g.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if _, err := look("docker"); err != nil {
		return errf("docker_not_found", "docker was not found on PATH. Install Docker Engine (https://docs.docker.com/engine/install/), then install gVisor: %s", GVisorInstallFix)
	}
	out, stderr, err := g.docker(ctx, nil, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return errf("docker_unavailable", "cannot talk to the Docker daemon: %v: %s. Check that Docker is running and that your user may use it (for example: sudo usermod -aG docker $USER, then log in again)", err, strings.TrimSpace(stderr))
	}
	var rts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rts); err != nil {
		return errf("docker_unavailable", "unexpected output from docker info: %q", strings.TrimSpace(out))
	}
	if _, ok := rts["runsc"]; !ok {
		return errf("runsc_not_found", "the runsc runtime is not registered with Docker. %s", GVisorInstallFix)
	}
	return nil
}

// gvBuildContext returns the tar build context and the tag derived from its content.
func gvBuildContext() ([]byte, string, error) {
	lf := func(s string) []byte { return []byte(strings.ReplaceAll(s, "\r\n", "\n")) }
	files := []struct {
		name string
		mode int64
		data []byte
	}{
		{"Dockerfile", 0o644, []byte(gvDockerfile)},
		{"dock-fast-agent.py", 0o755, lf(gvAgentProgram)},
		{"dock-entrypoint", 0o755, lf(gvEntrypoint)},
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), gvImageRepo + ":" + hex.EncodeToString(sum[:])[:12], nil
}

// GVisorImageTag is the image tag for the current build context.
func GVisorImageTag() (string, error) {
	_, tag, err := gvBuildContext()
	return tag, err
}

// EnsureImage builds the local image when it is missing and returns its tag. say is called
// once before a build starts.
func (g *GVisor) EnsureImage(ctx context.Context, say func(string)) (string, error) {
	tarball, tag, err := gvBuildContext()
	if err != nil {
		return "", errf("image_build_failed", "%v", err)
	}
	if _, _, err := g.docker(ctx, nil, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	if say != nil {
		say("building the gVisor dock image " + tag + " (first use only)")
	}
	if _, stderr, err := g.docker(ctx, bytes.NewReader(tarball), "build", "-t", tag, "-"); err != nil {
		return "", errf("image_build_failed", "docker build %s failed: %v\n%s", tag, err, lastLines(stderr, 15))
	}
	return tag, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func gvContainer(id string) string { return "dock-local-" + id }
func gvVolume(id string) string    { return "dock-local-" + id + "-work" }

// gvRunArgs returns the docker run arguments for one dock.
func gvRunArgs(rec Record, port int, agentConfig, image string) []string {
	return []string{
		"run", "-d", "--runtime=runsc", "--name", gvContainer(rec.ID),
		"-p", "127.0.0.1:" + strconv.Itoa(port) + ":" + strconv.Itoa(guestagent.Port),
		"--cpus", strconv.Itoa(rec.CPUs),
		"--memory", strconv.Itoa(rec.MemoryMiB) + "m",
		"--user", strconv.Itoa(gvUID) + ":" + strconv.Itoa(gvUID),
		"--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(gvPidsLimit),
		"--tmpfs", "/tmp",
		"-v", gvVolume(rec.ID) + ":/home/user/work",
		"-v", agentConfig + ":" + gvAgentConfigPath + ":ro",
		image,
	}
}

func notFound(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such volume") || strings.Contains(s, "no such object")
}

// running reports whether the dock's container is running.
func (g *GVisor) running(ctx context.Context, id string) bool {
	out, _, err := g.docker(ctx, nil, "inspect", "--format", "{{.State.Running}}", gvContainer(id))
	return err == nil && strings.TrimSpace(out) == "true"
}

// removeContainer force-removes the container; one that is already gone is not an error.
func (g *GVisor) removeContainer(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "rm", "-f", gvContainer(id)); err != nil && !notFound(stderr) {
		return errf("stop_failed", "docker rm %s: %v: %s", gvContainer(id), err, strings.TrimSpace(stderr))
	}
	return nil
}

func (g *GVisor) removeVolume(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "volume", "rm", "-f", gvVolume(id)); err != nil && !notFound(stderr) {
		return errf("remove_failed", "docker volume rm %s: %v: %s", gvVolume(id), err, strings.TrimSpace(stderr))
	}
	return nil
}

func (g *GVisor) logs(ctx context.Context, id string) string {
	out, errb, _ := g.docker(ctx, nil, "logs", "--tail", "12", gvContainer(id))
	if s := strings.TrimSpace(out + errb); s != "" {
		return "\n--- container logs ---\n" + s
	}
	return ""
}

func (e *Engine) gvisor() *GVisor {
	if e.GVisor != nil {
		return e.GVisor
	}
	return NewGVisor()
}

// gvPrepare is the preflight and image step shared by new and resume.
func (e *Engine) gvPrepare(ctx context.Context) (string, error) {
	g := e.gvisor()
	if err := g.Preflight(ctx); err != nil {
		return "", err
	}
	return g.EnsureImage(ctx, func(s string) { e.logf("%s", s) })
}

// bootGVisor runs a fresh container for rec on its work volume and waits for its agent.
func (e *Engine) bootGVisor(ctx context.Context, rec Record, image string) error {
	g := e.gvisor()
	tok, err := e.token(rec.ID)
	if err != nil {
		return err
	}
	cfg := e.file(rec.ID, "fast-agent.json")
	if err := WriteGuestConfig(cfg, tok, "", GuestOptions{}); err != nil {
		return errf("store_error", "%v", err)
	}
	// The container user (uid 1001) must read the bind-mounted file, so it cannot be 0600;
	// the dock directory is 0700, so other host users still cannot reach it.
	if err := os.Chmod(cfg, 0o644); err != nil {
		return errf("store_error", "%v", err)
	}
	for attempt := 1; ; attempt++ {
		port, err := freePort()
		if err != nil {
			return errf("start_failed", "no free local port: %v", err)
		}
		if err := os.WriteFile(e.file(rec.ID, "port"), []byte(strconv.Itoa(port)), 0o600); err != nil {
			return errf("store_error", "%v", err)
		}
		// A leftover container (crash, failed earlier attempt) would block the name.
		if err := g.removeContainer(ctx, rec.ID); err != nil {
			return err
		}
		_, stderr, err := g.docker(ctx, nil, gvRunArgs(rec, port, cfg, image)...)
		if err != nil {
			_ = g.removeContainer(ctx, rec.ID)
			low := strings.ToLower(stderr)
			if attempt < bootAttempts && (strings.Contains(low, "port is already allocated") || strings.Contains(low, "address already in use")) {
				e.logf("port taken, retrying with another one")
				continue
			}
			return errf("boot_failed", "docker run failed: %v: %s", err, strings.TrimSpace(stderr))
		}
		return e.waitReadyGVisor(ctx, rec.ID)
	}
}

func (e *Engine) waitReadyGVisor(ctx context.Context, id string) error {
	g := e.gvisor()
	fail := func(code, format string, a ...any) error {
		logs := g.logs(ctx, id)
		_ = g.removeContainer(context.WithoutCancel(ctx), id)
		return errf(code, format+"%s", append(a, logs)...)
	}
	deadline := time.NewTimer(e.bootTimeout())
	defer deadline.Stop()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if !g.running(ctx, id) {
			return fail("boot_failed", "the container exited during boot.")
		}
		if e.ping(ctx, id) {
			return nil
		}
		select {
		case <-ctx.Done():
			_ = g.removeContainer(context.WithoutCancel(ctx), id)
			return ctx.Err()
		case <-deadline.C:
			return fail("boot_timeout", "the guest agent did not answer within %s.", e.bootTimeout())
		case <-tick.C:
		}
	}
}

// stopGVisor removes the container and keeps the work volume.
func (e *Engine) stopGVisor(ctx context.Context, id string, force bool) error {
	g := e.gvisor()
	if !force && g.running(ctx, id) {
		_, stderr, err := g.docker(ctx, nil, "stop", "--time", "10", gvContainer(id))
		if err != nil && !notFound(stderr) {
			// rm -f below still ends it.
			e.logf("docker stop: %v", err)
		}
	}
	if err := g.removeContainer(ctx, id); err != nil {
		return err
	}
	e.clearRuntime(id)
	return nil
}

// newGVisor creates a gVisor dock and boots it.
func (e *Engine) newGVisor(ctx context.Context, spec Spec, diskSet bool) (Dock, error) {
	if spec.Network == "off" {
		return Dock{}, errf("not_supported", "--network off is not supported with --runtime gvisor: a published port needs a network")
	}
	image, err := e.gvPrepare(ctx)
	if err != nil {
		return Dock{}, err
	}
	if diskSet {
		e.logf("note: --disk is not enforced for gvisor docks; the work volume grows with the Docker data directory")
	}
	rec := Record{ID: NewID(), Name: spec.Name, Image: "dock-local-gvisor", Runtime: RuntimeGVisor, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB,
		DiskGiB: spec.DiskGiB, Network: spec.Network, CreatedAt: e.now()}
	if err := e.mkDockDir(rec.ID); err != nil {
		return Dock{}, errf("store_error", "%v", err)
	}
	if err := os.WriteFile(e.file(rec.ID, "token"), []byte(NewToken()), 0o600); err != nil {
		_ = e.removeFiles(rec.ID)
		return Dock{}, errf("store_error", "%v", err)
	}
	if err := e.addRecord(rec); err != nil {
		_ = e.removeFiles(rec.ID)
		return Dock{}, err
	}
	e.logf("starting %s", rec.ID)
	if err := e.bootGVisor(ctx, rec, image); err != nil {
		_ = e.Remove(context.WithoutCancel(ctx), rec.ID)
		return Dock{}, err
	}
	return e.view(ctx, rec), nil
}

// gvUnsupported is the error for verbs a gVisor dock cannot do.
func gvUnsupported(verb string) error {
	return errf("not_supported", "%s is not supported for gvisor docks (only /home/user/work persists; there is no disk image to copy)", verb)
}
