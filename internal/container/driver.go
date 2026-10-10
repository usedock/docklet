// Package container runs gvisor docks as Docker containers under the gVisor
// runsc runtime. It satisfies fc.Driver (and host.Backend) so orchestration
// above it — allocator, cgroup limits, pool, reconciler — stays unchanged.
package container

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	dockexec "github.com/usedock/docklet/internal/exec"
	"github.com/usedock/docklet/internal/fc"
)

// Runtime is the pinned gVisor runtime for every gvisor container.
const Runtime = "runsc"

// DockUID/DockGID identify the image's unprivileged `user`, which runs both
// the dock and its agent. Containers never run as root: --cap-drop=ALL drops
// CAP_DAC_OVERRIDE, so root cannot even traverse /home/user to reach the
// image workdir, and a root dock would defeat the isolation story.
const (
	DockUID = 1001
	DockGID = 1001
)

// Driver starts containers through the Docker CLI; no daemon SDK needed.
type Driver struct {
	Runner dockexec.Runner
	Image  string
	// InspectPID resolves a container to its init pid; nil shells docker.
	InspectPID func(container string) (int, error)
	// dockerExec runs `docker <args>` with the given streams; nil shells docker.
	dockerExec func(args []string, stdin io.Reader, stdout io.Writer) error
	// hostTar runs tar on the host; nil shells tar.
	hostTar func(args []string) error
	// dockerOut runs `docker <args>` and returns stdout; nil shells docker.
	dockerOut func(args []string) ([]byte, error)
	// untar unpacks a tar stream into a container; nil shells docker.
	untar func(container string, r io.Reader) error
}

func (d *Driver) runtime() string {
	return Runtime
}

func (d *Driver) runner() dockexec.Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return dockexec.Run
}

// ContainerName derives a collision-free docker name from a hash of the full dock id.
func ContainerName(dockID string) string {
	sum := sha256.Sum256([]byte(dockID))
	return "dock-" + hex.EncodeToString(sum[:16])
}

// sanitizedName is the readable id-derived name that keys the workspace volume.
func sanitizedName(dockID string) string {
	var b strings.Builder
	b.WriteString("dock-")
	for _, r := range dockID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// UseVeth marks veth networking; host.go branches off it.
func (d *Driver) UseVeth() bool { return true }

// DiskOnly marks disk-only generations; host.go skips memory captures.
func (d *Driver) DiskOnly() bool { return true }

// NetHolderName names the runc container that owns a dock's network namespace.
func NetHolderName(dockID string) string { return ContainerName(dockID) + "-net" }

// Boot starts one gvisor container with the dock's resource limits and the default locked
// profile: runsc runtime, read-only rootfs with a writable workspace volume,
// the unprivileged dock user, no privileged flag, no added caps. gVisor only
// sees interfaces present at sandbox start, so the network is wired into a
// holder's namespace first and the dock joins it; without AttachNetwork the
// dock has no network.
func (d *Driver) Boot(_ context.Context, spec fc.BootSpec) (*fc.Handle, error) {
	if err := fc.ValidateResources(spec.Resources); err != nil {
		return nil, err
	}
	if d.Image == "" {
		return nil, fmt.Errorf("container image not configured")
	}
	name := ContainerName(spec.DockID)
	workVol := WorkVolume(spec.DockID)
	image := d.Image
	network := "--network=none"
	if spec.AttachNetwork != nil {
		holder, err := d.startNetHolder(spec.DockID, spec.AttachNetwork)
		if err != nil {
			return nil, err
		}
		network = "--network=container:" + holder
	}
	args := []string{
		"run", "-d", "--name", name,
		"--runtime=" + d.runtime(),
		network,
		"--cpus", strconv.Itoa(spec.Resources.CPUs),
		"--memory", fmt.Sprintf("%dm", spec.Resources.MemoryMiB),
		"--user", fmt.Sprintf("%d:%d", DockUID, DockGID),
		"--tmpfs", "/tmp",
		"-v", workVol + ":/home/user/work",
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
	}
	// The host stages the pool claim token beside the sidecar; mount it read-only.
	if host := filepath.Join(spec.JailRoot, "fast-agent.json"); fileExists(host) {
		args = append(args, "-v", host+":"+apiv1.FastAgentConfigPath+":ro")
	}
	args = append(args, image)
	if err := d.runner()("docker", args...); err != nil {
		d.removeNetHolder(spec.DockID)
		return nil, fmt.Errorf("docker run: %w", err)
	}
	if err := os.MkdirAll(spec.JailRoot, 0o755); err != nil {
		return nil, err
	}
	if err := fc.WriteSidecar(spec.JailRoot, spec.DockID, spec.Resources); err != nil {
		return nil, err
	}
	pid, err := d.inspect(name)
	if err != nil {
		return nil, err
	}
	return &fc.Handle{DockID: spec.DockID, JailRoot: spec.JailRoot, Pid: pid}, nil
}

// guestDNS is the public resolver gvisor docks use, matching the microVM network unit;
// the host resolver is usually a private address guests may not reach.
const guestDNS = "1.1.1.1"

// startNetHolder runs a capless runc sleeper with no network, lets attach wire
// its namespace, and returns its name for the dock to join.
func (d *Driver) startNetHolder(dockID string, attach func(pid int) error) (string, error) {
	holder := NetHolderName(dockID)
	// A crashed boot can leave a holder behind; its namespace is stale.
	d.removeNetHolder(dockID)
	if err := d.runner()("docker", "run", "-d", "--name", holder,
		"--runtime=runc", "--network=none",
		"--dns", guestDNS, "--dns-search", ".",
		"--user", "65534:65534", "--read-only",
		"--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--entrypoint", "sleep", d.Image, "infinity"); err != nil {
		return "", fmt.Errorf("docker run network holder: %w", err)
	}
	pid, err := d.inspect(holder)
	if err == nil {
		err = attach(pid)
	}
	if err != nil {
		d.removeNetHolder(dockID)
		return "", fmt.Errorf("attach network: %w", err)
	}
	return holder, nil
}

func (d *Driver) removeNetHolder(dockID string) {
	_ = d.runner()("docker", "rm", "-f", NetHolderName(dockID))
}

// Adopt reattaches to a container that outlived a restart via its sidecar.
func (d *Driver) Adopt(dockID, jailRoot string, res apiv1.Resources) (*fc.Handle, error) {
	if err := fc.ValidateResources(res); err != nil {
		return nil, err
	}
	pid, err := d.inspect(ContainerName(dockID))
	if err != nil {
		return nil, err
	}
	return &fc.Handle{DockID: dockID, JailRoot: jailRoot, Pid: pid}, nil
}

// Pause freezes container processes; a missing container degrades to
// ErrNoMicroVM so callers fall back to disk-level work. An already-paused
// container is the desired state, so a retried capture after a failed attempt
// proceeds instead of failing on the leftover freeze.
func (d *Driver) Pause(_ context.Context, h *fc.Handle) error {
	if err := d.runner()("docker", "pause", ContainerName(h.DockID)); err != nil {
		if isMissing(err) {
			return fmt.Errorf("%w: %s", fc.ErrNoMicroVM, h.DockID)
		}
		if isAlreadyPaused(err) {
			return nil
		}
		return err
	}
	return nil
}

// Resume continues a paused container; a running container is already there.
func (d *Driver) Resume(_ context.Context, h *fc.Handle) error {
	if err := d.runner()("docker", "unpause", ContainerName(h.DockID)); err != nil {
		if isNotPaused(err) {
			return nil
		}
		return err
	}
	return nil
}

// isAlreadyPaused matches docker refusing a redundant freeze.
func isAlreadyPaused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is already paused")
}

// isNotPaused matches docker refusing to thaw a container that is running.
func isNotPaused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not paused")
}

// CreateSnapshot is a no-op: container generations are disk-only and the
// host skips memory captures for DiskOnly drivers.
func (d *Driver) CreateSnapshot(_ context.Context, _ *fc.Handle, _ string) error {
	return nil
}

// SnapshotPaths exposes no memory files for disk-only generations.
func (d *Driver) SnapshotPaths(h *fc.Handle) (string, string) {
	return filepath.Join(h.JailRoot, "snap.mem"), filepath.Join(h.JailRoot, "snap.state")
}

// Terminate force-removes the container, keeping its workspace volume and
// jail sidecar for resume/branch. A paused runsc container can be killed
// without being deleted in the same call, which would leave it pinning the
// workspace volume and block every later restore, so removal is retried
// until the container is actually gone.
func (d *Driver) Terminate(h *fc.Handle) error {
	var err error
	for attempt := range terminateAttempts {
		if attempt > 0 {
			time.Sleep(terminateBackoff)
		}
		err = d.removeContainers(h.DockID)
		if err == nil {
			d.removeNetHolder(h.DockID)
			return nil
		}
	}
	return err
}

// removeContainers force-removes the dock container; absence is success.
func (d *Driver) removeContainers(dockID string) error {
	if err := d.runner()("docker", "rm", "-f", ContainerName(dockID)); err != nil && !isMissing(err) {
		return err
	}
	return nil
}

// terminateAttempts/Backoff bound how long a removal keeps retrying: a
// container that is genuinely stuck must not hang the caller forever.
const (
	terminateAttempts = 5
	terminateBackoff  = 500 * time.Millisecond
)

// WorkVolume names the workspace volume backing a dock. Dock state lives on
// this volume so a capture is a host-side tar of one directory rather than a
// copy of the whole container filesystem.
func WorkVolume(dockID string) string { return sanitizedName(dockID) + "-work" }

// WorkVolume names the workspace volume for a dock through the driver, so the
// orchestrator never has to know how the name is derived.
func (d *Driver) WorkVolume(dockID string) string { return WorkVolume(dockID) }

// VolumeDir resolves a volume's host directory through the daemon, which is
// the only supported way to locate it.
func (d *Driver) VolumeDir(volume string) (string, error) {
	out, err := d.execOut("volume", "inspect", "-f", "{{.Mountpoint}}", volume)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("volume %s has no mountpoint", volume)
	}
	return dir, nil
}

// EnsureVolume creates the workspace volume if absent, so a restore can write
// into it before the container exists.
func (d *Driver) EnsureVolume(volume string) error {
	if err := d.runner()("docker", "volume", "create", volume); err != nil {
		return fmt.Errorf("docker volume create: %w", err)
	}
	return nil
}

// SaveState tars a state directory to dest, recording paths relative to the
// directory so a restore reproduces the same layout.
func (d *Driver) SaveState(dir, dest string) error {
	return d.runHostTar([]string{"-C", dir, "-cf", dest, "."})
}

// RestoreState unpacks a state tarball into a directory, replacing whatever is
// there so a restore never leaves stale files behind.
func (d *Driver) RestoreState(dir, tarPath string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := d.runHostTar([]string{"-C", dir, "-xf", tarPath}); err != nil {
		return err
	}
	// The dock runs unprivileged and owns everything it created, so restore
	// the same ownership the live workspace had.
	return d.runner()("chown", "-R", fmt.Sprintf("%d:%d", DockUID, DockGID), dir)
}

// runHostTar runs tar on the host: dock state is a host directory, so no
// container exec is involved and no in-container permission applies.
func (d *Driver) runHostTar(args []string) error {
	if d.hostTar != nil {
		return d.hostTar(args)
	}
	cmd := exec.Command("tar", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runDocker runs one `docker <args>` invocation, folding stderr into the error.
func (d *Driver) runDocker(args []string, stdin io.Reader, stdout io.Writer) error {
	if d.dockerExec != nil {
		return d.dockerExec(args, stdin, stdout)
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// execOut runs one docker subcommand and returns its stdout, folding stderr
// into the error so a failure is diagnosable from the control plane.
func (d *Driver) execOut(args ...string) ([]byte, error) {
	if d.dockerOut != nil {
		return d.dockerOut(args)
	}
	cmd := exec.Command("docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (d *Driver) inspect(container string) (int, error) {
	if d.InspectPID != nil {
		return d.InspectPID(container)
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", container).Output()
	if err != nil {
		return 0, fmt.Errorf("docker inspect %s: %w", container, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("docker inspect %s: bad pid %q", container, strings.TrimSpace(string(out)))
	}
	return pid, nil
}

func isMissing(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "No such container") || strings.Contains(msg, "no such container")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
