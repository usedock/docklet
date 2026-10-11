// Package v1 is the versioned docklet HTTP contract. The control plane is
// the only client; every mutating call carries an idempotency token so
// workflow replay never executes twice.
package v1

import "encoding/json"

// DockletFeatures is what this build advertises at registration; the control plane gates behaviour on it.
var DockletFeatures = []string{"egress-on-resume"}

// APIVersion is the docklet contract version served under /v1/*.
const APIVersion = "v1"

// FastAgentConfigPath is where fast runtimes receive their claim token.
const FastAgentConfigPath = "/etc/dock-fast-agent.json"

// DefaultImage is the only image the docklet serves today.
const DefaultImage = "ubuntu-24.04"

// Runtime names what isolates a dock on a host.
const (
	RuntimeMicroVM = "microvm"
	RuntimeGVisor  = "gvisor"
)

// Envelope mirrors the control-plane shape: ok/data on success,
// ok false with a stable snake_case code on failure.
type Envelope struct {
	Ok      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// Resources is a dock's requested size; cgroup limits and disk come from it.
type Resources struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
}

// ProvisionRequest boots one microVM for a dock. SnapshotID pins a
// generation to fork from; empty boots the base image fresh.
type ProvisionRequest struct {
	DockID     string    `json:"dockId"`
	Resources  Resources `json:"resources"`
	Image      string    `json:"image"`
	SnapshotID string    `json:"snapshotId,omitempty"`
	ExecAgent  bool      `json:"execAgent,omitempty"`
	// EgressAllow confines the guest to these HTTPS hosts ("*.x" for subdomains); requires ExecAgent.
	EgressAllow      []string `json:"egressAllow,omitempty"`
	IdempotencyToken string   `json:"idempotencyToken"`
}

// ProvisionResponse carries the reachable guest address.
type ProvisionResponse struct {
	IP     string `json:"ip"`
	HostID string `json:"hostId"`
}

// PauseRequest stops the microVM without snapshotting; stop flows
// snapshot first via SnapshotCreate, then pause, then destroy on delete.
type PauseRequest struct {
	DockID           string `json:"dockId"`
	IdempotencyToken string `json:"idempotencyToken"`
}

// SnapshotKind selects a full memory+device capture or a diff layer.
type SnapshotKind string

const (
	SnapshotFull SnapshotKind = "full"
	SnapshotDiff SnapshotKind = "diff"
)

// SnapshotCreateRequest captures one generation into the snapshot store.
// Captures are disk-first: the frozen rootfs is the restorable artifact and
// resume always fresh-boots (reboot semantics). Memory capture is opt-in via
// Memory for future fast-fork; Firecracker memory snapshots are DevPreview
// and have proven fatal to the snapshotted VM on this stack, so the default
// flows leave them off.
type SnapshotCreateRequest struct {
	DockID           string       `json:"dockId"`
	ParentID         *string      `json:"parentId"`
	Kind             SnapshotKind `json:"kind"`
	ResumeAfter      bool         `json:"resumeAfter"`
	Memory           bool         `json:"memory"`
	IdempotencyToken string       `json:"idempotencyToken"`
}

// SnapshotCreateResponse identifies the stored generation.
type SnapshotCreateResponse struct {
	SnapshotID string `json:"snapshotId"`
	SizeBytes  int64  `json:"sizeBytes"`
	CPUFamily  string `json:"cpuFamily"`
	// BaseFingerprint names the base image a microvm delta generation restores
	// onto; empty for gvisor and whole-image generations.
	BaseFingerprint string `json:"baseFingerprint,omitempty"`
}

// SnapshotLoadRequest restores a generation onto a fresh microVM at the
// given resources.
type SnapshotLoadRequest struct {
	DockID     string    `json:"dockId"`
	Resources  Resources `json:"resources"`
	SnapshotID string    `json:"snapshotId"`
	// ExecAgent boots the guest exec agent with a fresh token.
	ExecAgent bool `json:"execAgent,omitempty"`
	// EgressAllow confines the resumed guest to these HTTPS hosts; requires ExecAgent.
	EgressAllow      []string `json:"egressAllow,omitempty"`
	IdempotencyToken string   `json:"idempotencyToken"`
}

// SnapshotLoadResponse carries the restored guest address.
type SnapshotLoadResponse struct {
	IP     string `json:"ip"`
	HostID string `json:"hostId"`
}

// DestroyRequest fences the microVM and releases its slot.
type DestroyRequest struct {
	DockID           string `json:"dockId"`
	IdempotencyToken string `json:"idempotencyToken"`
}

// FastRunCapabilities is the versioned provider contract advertised by a
// warm ephemeral microVM. Capability names are policy inputs, not backend names.
type FastRunCapabilities struct {
	Version      int             `json:"version"`
	Target       string          `json:"target"`
	Profile      string          `json:"profile"`
	Capabilities map[string]bool `json:"capabilities"`
}

// FastRunCreateRequest allocates one disposable runtime and its guest agent.
type FastRunCreateRequest struct {
	RuntimeID        string              `json:"runtimeId"`
	Resources        Resources           `json:"resources"`
	Image            string              `json:"image"`
	IdempotencyToken string              `json:"idempotencyToken"`
	GuestToken       string              `json:"guestToken"`
	Capabilities     FastRunCapabilities `json:"capabilities"`
	// EgressAllow confines the runtime to these HTTPS hosts; empty leaves egress open.
	EgressAllow []string `json:"egressAllow,omitempty"`
}

// FastRunCreateResponse identifies the runtime and its resolved capabilities.
type FastRunCreateResponse struct {
	RuntimeID    string              `json:"runtimeId"`
	IP           string              `json:"ip"`
	HostID       string              `json:"hostId"`
	Capabilities FastRunCapabilities `json:"capabilities"`
}

// FastRunExecRequest executes a bounded shell script inside the runtime.
type FastRunExecRequest struct {
	RuntimeID        string            `json:"runtimeId"`
	Script           string            `json:"script"`
	TimeoutSeconds   int               `json:"timeoutSeconds"`
	Env              map[string]string `json:"env,omitempty"`
	IdempotencyToken string            `json:"idempotencyToken"`
}

// FastRunExecResponse contains buffered stdout/stderr for the bounded script.
type FastRunExecResponse struct {
	RuntimeID string `json:"runtimeId"`
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	// Truncated reports that stdout or stderr was clipped to its head.
	Truncated bool `json:"truncated"`
	// TimedOut reports that the script was killed at its budget.
	TimedOut bool `json:"timedOut"`
}

// FastRunReadRequest reads one artifact path before runtime cleanup.
type FastRunReadRequest struct {
	RuntimeID        string `json:"runtimeId"`
	Path             string `json:"path"`
	IdempotencyToken string `json:"idempotencyToken"`
}

// FastRunReadResponse contains the artifact bytes encoded by JSON.
type FastRunReadResponse struct {
	RuntimeID string `json:"runtimeId"`
	Path      string `json:"path"`
	Content   []byte `json:"content"`
	SizeBytes int64  `json:"sizeBytes"`
}

// FastRunTerminateRequest destroys the runtime and revokes its guest token.
type FastRunTerminateRequest struct {
	RuntimeID        string `json:"runtimeId"`
	IdempotencyToken string `json:"idempotencyToken"`
}

// ContainerSample is best-effort guest Docker enrichment, never billing truth.
type ContainerSample struct {
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemBytes   int64   `json:"memBytes"`
}

// StatsSample is one host-collected observation for a dock.
type StatsSample struct {
	DockID     string  `json:"dockId"`
	At         string  `json:"at"`
	Source     string  `json:"source"`
	CPUPercent float64 `json:"cpuPercent"`
	MemBytes   int64   `json:"memBytes"`
	MemLimit   int64   `json:"memLimit"`
	DiskBytes  int64   `json:"diskBytes"`
	// DiskUsedBytes is space the dock's writable disk holds on the host.
	DiskUsedBytes int64             `json:"diskUsedBytes"`
	RxBytes       int64             `json:"rxBytes"`
	TxBytes       int64             `json:"txBytes"`
	Containers    []ContainerSample `json:"containers,omitempty"`
}

// RunningVM is one microVM in the host inventory heartbeat.
type RunningVM struct {
	DockID string `json:"dockId"`
	State  string `json:"state"`
	// Kind is dock for persistent docks, fast for ephemeral agent runs.
	Kind string `json:"kind,omitempty"`
}

// HeartbeatRequest advertises capacity and inventory for placement.
type HeartbeatRequest struct {
	HostID     string      `json:"hostId"`
	Address    string      `json:"address"`
	CPUFamily  string      `json:"cpuFamily"`
	Kernel     string      `json:"kernel"`
	VcpuTotal  int         `json:"vcpuTotal"`
	VcpuFree   int         `json:"vcpuFree"`
	MemTotal   int64       `json:"memTotalBytes"`
	MemFree    int64       `json:"memFreeBytes"`
	DiskTotal  int64       `json:"diskTotalBytes"`
	DiskFree   int64       `json:"diskFreeBytes"`
	RunningVMs []RunningVM `json:"runningVms"`
}

// Readiness enumerates host prerequisites; empty Reasons means ready.
type Readiness struct {
	Ready   bool     `json:"ready"`
	Reasons []string `json:"reasons,omitempty"`
}

// The Fleet* types are the outbound channel: the docklet is the client and
// the control plane's /v1/fleet/* routes reject unknown fields.

// FleetRegisterRequest announces this host's capacity for placement.
type FleetRegisterRequest struct {
	HostID         string `json:"hostId"`
	CPUFamily      string `json:"cpuFamily"`
	Kernel         string `json:"kernel"`
	KernelRelease  string `json:"kernelRelease"`
	Runtime        string `json:"runtime"`
	VcpuTotal      int    `json:"vcpuTotal"`
	MemTotalBytes  int64  `json:"memTotalBytes"`
	MemBudgetBytes *int64 `json:"memBudgetBytes,omitempty"`
	DiskTotalBytes int64  `json:"diskTotalBytes"`
	MaxDocks       int    `json:"maxDocks"`
	// AcceptsCredential tells the control plane this docklet stores the per-host
	// credential, so it may issue one; older docklets stay credential-less.
	AcceptsCredential bool `json:"acceptsCredential,omitempty"`
	// Features lists optional docklet behaviours (such as "egress-on-resume") the control plane may rely on.
	Features []string `json:"features,omitempty"`
}

// FleetRegisterResponse carries the cadence the control plane wants from this host.
type FleetRegisterResponse struct {
	HostID           string `json:"hostId"`
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
	PollWaitSeconds  int    `json:"pollWaitSeconds"`
	// Credential is the per-host secret, returned only to the enrollment token.
	Credential string `json:"credential,omitempty"`
}

// FleetHeartbeatRequest refreshes this host's inventory; the control plane
// treats running as the reconciler's source of truth.
type FleetHeartbeatRequest struct {
	HostID    string        `json:"hostId"`
	UsedDocks int           `json:"usedDocks"`
	Runtime   string        `json:"runtime,omitempty"`
	Running   []RunningVM   `json:"running"`
	Samples   []StatsSample `json:"samples,omitempty"`
	// BaseFingerprints is a pointer so a microvm host holding no base still reports [].
	BaseFingerprints *[]string `json:"baseFingerprints,omitempty"`
	// Images names what this host serves: the built base plus pulled Dockyard
	// images. A pointer so a host serving none still reports [].
	Images *[]string `json:"images,omitempty"`
}

// FleetPollResponse is the long-poll answer; Command is nil when none is queued.
type FleetPollResponse struct {
	Command *FleetCommand `json:"command"`
}

// FleetCommand is one queued unit of work for this host.
type FleetCommand struct {
	ID               string          `json:"id"`
	Op               string          `json:"op"`
	IdempotencyToken string          `json:"idempotencyToken"`
	DeadlineMs       int64           `json:"deadlineMs"`
	Payload          json.RawMessage `json:"payload"`
}

// FleetResultRequest is one command outcome posted back to the control plane.
type FleetResultRequest struct {
	HostID  string          `json:"hostId"`
	Ok      bool            `json:"ok"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}
