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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// Docker runtime names a gvisor dock can use. runsc-suid is a second registration of the same
// runsc binary with --allow-suid (gVisor ignores setuid bits otherwise, so sudo fails); it is a
// separate runtime so other runsc containers on the machine keep the default.
const (
	gvRuntimePlain = "runsc"
	gvRuntimeSUID  = "runsc-suid"
)

// GVisorSUIDFix is how to enable sudo inside new gvisor docks, once.
const GVisorSUIDFix = "sudo runsc install --runtime=runsc-suid -- --allow-suid && sudo systemctl restart docker"

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
	_, err := g.Probe(ctx)
	return err
}

// dockerRuntime is one entry of docker info Runtimes.
type dockerRuntime struct {
	Path        string   `json:"path"`
	RuntimeArgs []string `json:"runtimeArgs"`
}

// suidRuntime reports whether rt is a usable runsc-suid: a runsc binary registered with
// --allow-suid. One without the flag is treated as missing, because sudo would silently fail.
func suidRuntime(rt json.RawMessage) bool {
	var r dockerRuntime
	if json.Unmarshal(rt, &r) != nil || !strings.HasPrefix(filepath.Base(r.Path), "runsc") {
		return false
	}
	return slices.ContainsFunc(r.RuntimeArgs, func(a string) bool { return a == "--allow-suid" || a == "--allow-suid=true" })
}

// Probe runs the preflight and reports whether the runsc-suid runtime (sudo in docks) is available.
func (g *GVisor) Probe(ctx context.Context) (suid bool, err error) {
	if g.GOOS != "linux" {
		return false, errf("unsupported_os", "--runtime gvisor is only supported on Linux, not %s", g.GOOS)
	}
	look := g.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if _, err := look("docker"); err != nil {
		return false, errf("docker_not_found", "docker was not found on PATH. Install Docker Engine (https://docs.docker.com/engine/install/), then install gVisor: %s", GVisorInstallFix)
	}
	out, stderr, err := g.docker(ctx, nil, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return false, errf("docker_unavailable", "cannot talk to the Docker daemon: %v: %s. Check that Docker is running and that your user may use it (for example: sudo usermod -aG docker $USER, then log in again)", err, strings.TrimSpace(stderr))
	}
	var rts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rts); err != nil {
		return false, errf("docker_unavailable", "unexpected output from docker info: %q", strings.TrimSpace(out))
	}
	if _, ok := rts[gvRuntimePlain]; !ok {
		return false, errf("runsc_not_found", "the runsc runtime is not registered with Docker. %s", GVisorInstallFix)
	}
	return suidRuntime(rts[gvRuntimeSUID]), nil
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
func gvNetwork(id string) string   { return "dock-local-" + id + "-net" }

// gvRunArgs returns the docker run arguments for one dock. gVisor is the sandbox boundary, so
// root inside the dock (sudo) is sandbox root: Docker default capabilities stay, never --privileged.
func gvRunArgs(rec Record, port int, agentConfig, resolvConf, image string) []string {
	return []string{
		"run", "-d", "--runtime=" + rec.gvRuntime(), "--name", gvContainer(rec.ID),
		"--network", gvNetwork(rec.ID),
		"-p", "127.0.0.1:" + strconv.Itoa(port) + ":" + strconv.Itoa(guestagent.Port),
		"--cpus", strconv.Itoa(rec.CPUs),
		"--memory", strconv.Itoa(rec.MemoryMiB) + "m",
		"--user", strconv.Itoa(gvUID) + ":" + strconv.Itoa(gvUID),
		"--pids-limit", strconv.Itoa(gvPidsLimit),
		"--tmpfs", "/tmp",
		"-v", gvVolume(rec.ID) + ":/home/user/work",
		"-v", agentConfig + ":" + gvAgentConfigPath + ":ro",
		"-v", resolvConf + ":/etc/resolv.conf:ro",
		image,
	}
}

func notFound(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such volume") || strings.Contains(s, "no such object") ||
		strings.Contains(s, "no such network") || (strings.Contains(s, "network") && strings.Contains(s, "not found"))
}

// dockerFailure turns a failed docker call into a coded error; an unreachable daemon is
// docker_unavailable (start Docker and retry) whatever the verb was.
func dockerFailure(code, what string, err error, stderr string) error {
	low := strings.ToLower(stderr)
	if strings.Contains(low, "cannot connect to the docker daemon") || strings.Contains(low, "error during connect") ||
		strings.Contains(low, "permission denied while trying to connect") {
		return errf("docker_unavailable", "cannot talk to the Docker daemon to %s: %s. Start Docker (or fix access to it) and retry", what, strings.TrimSpace(stderr))
	}
	return errf(code, "%s: %v: %s", what, err, strings.TrimSpace(stderr))
}

// hasDocker reports whether the docker CLI is on PATH.
func (g *GVisor) hasDocker() bool {
	look := g.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look("docker")
	return err == nil
}

// ensureNetwork creates the dock's own bridge network; one that already exists is fine.
// Each dock gets its own so it cannot reach the user's other containers or other docks.
func (g *GVisor) ensureNetwork(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "network", "create", "--driver", "bridge", gvNetwork(id)); err != nil && !strings.Contains(strings.ToLower(stderr), "already exists") {
		return dockerFailure("boot_failed", "docker network create "+gvNetwork(id), err, stderr)
	}
	return nil
}

func (g *GVisor) removeNetwork(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "network", "rm", gvNetwork(id)); err != nil && !notFound(stderr) {
		return dockerFailure("stop_failed", "docker network rm "+gvNetwork(id), err, stderr)
	}
	return nil
}

// disconnectNetwork detaches the (stopped) container from its network so the network can be
// removed while the dock is stopped; Docker address pools only hold a few dozen networks.
func (g *GVisor) disconnectNetwork(ctx context.Context, id string) error {
	_, stderr, err := g.docker(ctx, nil, "network", "disconnect", "--force", gvNetwork(id), gvContainer(id))
	if err != nil && !notFound(stderr) && !strings.Contains(strings.ToLower(stderr), "is not connected") {
		return dockerFailure("stop_failed", "docker network disconnect "+gvNetwork(id), err, stderr)
	}
	return nil
}

// connectNetwork attaches the stopped container to its freshly created network.
func (g *GVisor) connectNetwork(ctx context.Context, id string) error {
	_, stderr, err := g.docker(ctx, nil, "network", "connect", gvNetwork(id), gvContainer(id))
	if err != nil && !strings.Contains(strings.ToLower(stderr), "already exists") {
		return dockerFailure("boot_failed", "docker network connect "+gvNetwork(id), err, stderr)
	}
	return nil
}

// halt stops the container (keeping it and its rootfs) and releases its network.
func (g *GVisor) halt(ctx context.Context, id string, grace string) error {
	if _, stderr, err := g.docker(ctx, nil, "stop", "--time", grace, gvContainer(id)); err != nil && !notFound(stderr) {
		return dockerFailure("stop_failed", "docker stop "+gvContainer(id), err, stderr)
	}
	if err := g.disconnectNetwork(ctx, id); err != nil {
		return err
	}
	return g.removeNetwork(ctx, id)
}

// cleanup removes the container and the network, best effort, after a failed first start.
func (g *GVisor) cleanup(ctx context.Context, id string) {
	_ = g.removeContainer(ctx, id)
	_ = g.removeNetwork(ctx, id)
}

// gvFallbackDNS is what Docker itself uses when the host has no usable resolver.
var gvFallbackDNS = []string{"8.8.8.8", "8.8.4.4"}

// gvResolvConf builds the dock's resolv.conf from the host's IPv4 upstream nameservers.
// Loopback resolvers (systemd-resolved's 127.0.0.53) are unreachable from the container, so
// the real upstreams come from /run/systemd/resolve/resolv.conf, as Docker does.
func gvResolvConf(read func(string) ([]byte, error)) []byte {
	var servers []string
	for _, path := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		raw, err := read(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || f[0] != "nameserver" {
				continue
			}
			ip := net.ParseIP(f[1])
			if ip == nil || ip.To4() == nil || ip.IsLoopback() || slices.Contains(servers, f[1]) {
				continue
			}
			servers = append(servers, f[1])
		}
		if len(servers) > 0 {
			break
		}
	}
	if len(servers) == 0 {
		servers = gvFallbackDNS
	}
	var b strings.Builder
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	return []byte(b.String())
}

// running reports whether the dock's container is running.
func (g *GVisor) running(ctx context.Context, id string) bool {
	out, _, err := g.docker(ctx, nil, "inspect", "--format", "{{.State.Running}}", gvContainer(id))
	return err == nil && strings.TrimSpace(out) == "true"
}

// removeContainer force-removes the container; one that is already gone is not an error.
func (g *GVisor) removeContainer(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "rm", "-f", gvContainer(id)); err != nil && !notFound(stderr) {
		return dockerFailure("stop_failed", "docker rm "+gvContainer(id), err, stderr)
	}
	return nil
}

func (g *GVisor) removeVolume(ctx context.Context, id string) error {
	if _, stderr, err := g.docker(ctx, nil, "volume", "rm", "-f", gvVolume(id)); err != nil && !notFound(stderr) {
		return dockerFailure("remove_failed", "docker volume rm "+gvVolume(id), err, stderr)
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

// gvRuntime is the Docker runtime of a gvisor dock; older records have none and used runsc.
func (r Record) gvRuntime() string {
	if r.GVisorRuntime == "" {
		return gvRuntimePlain
	}
	return r.GVisorRuntime
}

// gvPrepare is the preflight and image step shared by new and resume. suid says whether the
// runsc-suid runtime is registered.
func (e *Engine) gvPrepare(ctx context.Context) (image string, suid bool, err error) {
	g := e.gvisor()
	suid, err = g.Probe(ctx)
	if err != nil {
		return "", false, err
	}
	image, err = g.EnsureImage(ctx, func(s string) { e.logf("%s", s) })
	return image, suid, err
}

// writeGVisorFiles writes the dock agent config and resolv.conf (both bind-mounted into the
// container) and returns their absolute paths. Rewriting them on resume refreshes the resolvers.
func (e *Engine) writeGVisorFiles(rec Record) (cfg, resolv string, err error) {
	tok, err := e.token(rec.ID)
	if err != nil {
		return "", "", err
	}
	// docker -v reads a relative source (DOCK_DATA_DIR=data) as a volume name, so pass an absolute path.
	cfg, err = filepath.Abs(e.file(rec.ID, "fast-agent.json"))
	if err != nil {
		return "", "", errf("store_error", "%v", err)
	}
	if err := WriteGuestConfig(cfg, tok, "", GuestOptions{}); err != nil {
		return "", "", errf("store_error", "%v", err)
	}
	// The container user (uid 1001) must read the bind-mounted file, so it cannot be 0600;
	// the dock directory is 0700, so other host users still cannot reach it.
	if err := os.Chmod(cfg, 0o644); err != nil {
		return "", "", errf("store_error", "%v", err)
	}
	// Docker points a user-defined network at its embedded DNS (127.0.0.11), which runsc's
	// own network stack cannot reach, so the dock gets the host upstream resolvers instead.
	resolv = filepath.Join(filepath.Dir(cfg), "resolv.conf")
	if err := os.WriteFile(resolv, gvResolvConf(os.ReadFile), 0o644); err != nil {
		return "", "", errf("store_error", "%v", err)
	}
	if err := os.Chmod(resolv, 0o644); err != nil {
		return "", "", errf("store_error", "%v", err)
	}
	return cfg, resolv, nil
}

// bootGVisor creates a fresh container for rec on its work volume and waits for its agent.
func (e *Engine) bootGVisor(ctx context.Context, rec Record, image string) error {
	g := e.gvisor()
	cfg, resolv, err := e.writeGVisorFiles(rec)
	if err != nil {
		return err
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
		if err := g.ensureNetwork(ctx, rec.ID); err != nil {
			return err
		}
		_, stderr, err := g.docker(ctx, nil, gvRunArgs(rec, port, cfg, resolv, image)...)
		if err != nil {
			g.cleanup(ctx, rec.ID)
			low := strings.ToLower(stderr)
			if attempt < bootAttempts && (strings.Contains(low, "port is already allocated") || strings.Contains(low, "address already in use")) {
				e.logf("port taken, retrying with another one")
				continue
			}
			return errf("boot_failed", "docker run failed: %v: %s", err, strings.TrimSpace(stderr))
		}
		return e.waitReadyGVisor(ctx, rec.ID, false)
	}
}

func gvPortInUse(port int) error {
	return errf("port_in_use", "local port %d, which this dock publishes its agent on, is now used by another program. Free it and run dock resume again", port)
}

// resumeGVisor starts the stopped container again, so the whole rootfs survives. Its published
// host port was fixed when it was created; if something else holds it now the error says so.
// A container that is gone (docker prune) is recreated on the work volume; only /home/user/work
// survives that.
func (e *Engine) resumeGVisor(ctx context.Context, rec Record, image string, suid bool) error {
	g := e.gvisor()
	if _, stderr, err := g.docker(ctx, nil, "inspect", "--format", "{{.State.Status}}", gvContainer(rec.ID)); err != nil {
		if !notFound(stderr) {
			return dockerFailure("boot_failed", "docker inspect "+gvContainer(rec.ID), err, stderr)
		}
		if rec.gvRuntime() == gvRuntimeSUID && !suid {
			return errf("runtime_missing", "dock %s was created with the %s runtime, which is no longer registered with Docker; register it again (%s) or remove the dock", rec.ID, gvRuntimeSUID, GVisorSUIDFix)
		}
		e.logf("the container of %s is gone; recreating it (only /home/user/work survives)", rec.ID)
		return e.bootGVisor(ctx, rec, image)
	}
	port := e.readInt(rec.ID, "port")
	if port <= 0 {
		return errf("dock_broken", "dock %s has no recorded port; remove it with: dock rm %s", rec.ID, rec.ID)
	}
	if _, _, err := e.writeGVisorFiles(rec); err != nil {
		return err
	}
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return gvPortInUse(port)
	}
	_ = l.Close()
	if err := g.ensureNetwork(ctx, rec.ID); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = g.halt(context.WithoutCancel(ctx), rec.ID, "0")
		return err
	}
	if err := g.connectNetwork(ctx, rec.ID); err != nil {
		return fail(err)
	}
	if _, stderr, err := g.docker(ctx, nil, "start", gvContainer(rec.ID)); err != nil {
		low := strings.ToLower(stderr)
		if strings.Contains(low, "port is already allocated") || strings.Contains(low, "address already in use") {
			return fail(gvPortInUse(port))
		}
		return fail(dockerFailure("boot_failed", "docker start "+gvContainer(rec.ID), err, stderr))
	}
	return e.waitReadyGVisor(ctx, rec.ID, true)
}

// waitReadyGVisor waits for the agent; on failure a first start removes the container, while a
// resume (keep) only stops it so the rootfs is not lost.
func (e *Engine) waitReadyGVisor(ctx context.Context, id string, keep bool) error {
	g := e.gvisor()
	undo := func(ctx context.Context) {
		if keep {
			_ = g.halt(ctx, id, "0")
			return
		}
		g.cleanup(ctx, id)
	}
	fail := func(code, format string, a ...any) error {
		logs := g.logs(ctx, id)
		undo(context.WithoutCancel(ctx))
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
			undo(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-deadline.C:
			return fail("boot_timeout", "the guest agent did not answer within %s.", e.bootTimeout())
		case <-tick.C:
		}
	}
}

// stopGVisor stops the container but keeps it, so its whole rootfs survives, and releases its
// network. The port file stays: the container keeps publishing the same host port.
func (e *Engine) stopGVisor(ctx context.Context, id string, force bool) error {
	grace := "10"
	if force {
		grace = "0"
	}
	return e.gvisor().halt(ctx, id, grace)
}

// removeGVisor deletes the container, its network and the work volume.
func (e *Engine) removeGVisor(ctx context.Context, id string) error {
	g := e.gvisor()
	if err := g.removeContainer(ctx, id); err != nil {
		return err
	}
	if err := g.removeNetwork(ctx, id); err != nil {
		return err
	}
	return g.removeVolume(ctx, id)
}

// newGVisor creates a gVisor dock and boots it.
func (e *Engine) newGVisor(ctx context.Context, spec Spec, diskSet bool) (Dock, error) {
	if spec.Network == "off" {
		return Dock{}, errf("not_supported", "--network off is not supported with --runtime gvisor: a published port needs a network")
	}
	image, suid, err := e.gvPrepare(ctx)
	if err != nil {
		return Dock{}, err
	}
	rt := gvRuntimePlain
	if suid {
		rt = gvRuntimeSUID
	} else {
		e.logf("note: sudo will not work in this dock (the %s runtime is not registered). To enable it once, for new docks: %s", gvRuntimeSUID, GVisorSUIDFix)
	}
	if diskSet {
		e.logf("note: --disk is not enforced for gvisor docks; the work volume grows with the Docker data directory")
	}
	rec := Record{ID: NewID(), Name: spec.Name, Image: "dock-local-gvisor", Runtime: RuntimeGVisor, GVisorRuntime: rt, CPUs: spec.CPUs, MemoryMiB: spec.MemoryMiB,
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
