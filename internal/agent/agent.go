// Package agent runs the docklet's outbound fleet control channel. A sandbox
// host sits behind NAT and never accepts inbound connections: the agent
// registers its capacity, refreshes inventory on a heartbeat ticker, and
// long-polls the control plane for commands. A command is executed by
// replaying it against the local docklet HTTP API, so the versioned contract
// (and its idempotency journal) stays the single implementation of docklet
// behaviour and the control plane stays the only other client.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/cpufamily"
	"github.com/usedock/docklet/internal/fleetauth"
	"github.com/usedock/docklet/internal/ids"
)

const (
	// DefaultLocalAPIBase is the loopback API commands are relayed into.
	DefaultLocalAPIBase = "http://127.0.0.1:8081"
	defaultHeartbeat    = 30 * time.Second
	defaultPollWait     = 25 * time.Second
	// callTimeout bounds register, heartbeat, and result calls; the poll and
	// relay paths carry their own budgets.
	callTimeout = 30 * time.Second
	// pollGrace covers long-poll transfer latency on top of the wait itself.
	pollGrace  = 15 * time.Second
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// The control plane bounds the diagnostic text it stores; a result whose
	// message exceeds that bound is rejected, which would strand the command,
	// so the agent truncates before posting.
	maxResultMessage = 8192
	// maxConcurrentExecs bounds guest execs and quick reads running beside lifecycle work.
	maxConcurrentExecs = 32
	// maxConcurrentLifecycle bounds lifecycle ops running at once across docks.
	maxConcurrentLifecycle = 8
	// maxPendingLifecycle bounds running plus queued lifecycle ops before the host refuses.
	maxPendingLifecycle = 64
	// deadlineHeader tells the local API the command budget so its handler wait ends with it.
	deadlineHeader = "X-Deadline-Ms"
)

// truncatedMarker makes a clipped diagnostic obvious to a reader.
const truncatedMarker = "... (truncated)"

// Orchestrator reports this host's live inventory for heartbeats.
type Orchestrator interface {
	Running() []apiv1.RunningVM
}

// Sampler enriches heartbeats with per-dock host stats. Optional: an
// orchestrator that does not sample simply reports inventory.
type Sampler interface {
	Stats(ctx context.Context, dockID string) (apiv1.StatsSample, error)
}

// Doer performs one HTTP request; tests inject a recording fake.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config wires one fleet agent. Host supplies the live inventory and, when it
// also implements Sampler, the heartbeat samples.
type Config struct {
	// FleetURL is the control plane base, e.g. https://api.example.com.
	FleetURL string
	// Token is the shared enrollment token, used to register and to obtain a
	// per-host credential; once issued, the credential carries every other call.
	Token string
	// Auth holds the per-host credential across restarts; nil keeps it in memory.
	Auth   *fleetauth.Credential
	HostID string
	// Runtime is the isolation runtime this host runs (microvm|gvisor); it is
	// reported verbatim and validated by the control plane, not here.
	Runtime string
	// MemoryReserveMiB is held back from guests for the host itself; 0 picks the larger of 1 GiB and 10% of RAM.
	MemoryReserveMiB int64
	// Family is the host's CPU family as registered with the orchestrator;
	// empty re-reads /proc/cpuinfo.
	Family string
	// LocalAPIBase is the loopback docklet API commands are relayed to.
	LocalAPIBase string
	// LocalToken is the bearer token the local API requires on relayed calls.
	LocalToken string
	Client     Doer
	Host       Orchestrator
	// Bases lists the base image fingerprints this host holds, reported on each
	// heartbeat so delta restores are placed only where their base exists; a nil
	// func (gvisor hosts) or a nil result reports nothing.
	Bases func() []string
	// OpenTerminal starts guest shells for terminal.open; nil refuses them.
	OpenTerminal TerminalOpener
	// OpenTunnel connects to dock ports for tunnel.open; nil refuses them.
	OpenTunnel TunnelOpener
	// Root prefixes host paths (/proc, the statfs target) for capacity
	// probing; empty means "/".
	Root string
	// HeartbeatInterval and PollWait default to the contract's 30s and 25s.
	// The register response overrides both.
	HeartbeatInterval time.Duration
	PollWait          time.Duration
	// Logf overrides the logger; tests capture operator-facing output here.
	Logf func(format string, args ...any)
}

// Run registers this host and then serves heartbeats and queued commands until
// ctx is cancelled. Cancellation stops every ticker and in-flight call, and
// return waits for the heartbeat goroutine so nothing is left running.
func (c Config) Run(ctx context.Context) error {
	if c.FleetURL == "" {
		return errors.New("docklet agent: fleet url required")
	}
	c.Auth = c.credential()
	if c.Auth.Bearer() == "" {
		return errors.New("docklet agent: fleet token or host credential required")
	}
	if c.HostID == "" {
		return errors.New("docklet agent: host id required")
	}
	if c.Host == nil {
		return errors.New("docklet agent: orchestrator required")
	}
	c.FleetURL = strings.TrimRight(c.FleetURL, "/")

	reg, err := c.register(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	every := c.HeartbeatInterval
	if every <= 0 {
		every = defaultHeartbeat
	}
	if reg.HeartbeatSeconds > 0 {
		every = time.Duration(reg.HeartbeatSeconds) * time.Second
	}
	waitFor := c.PollWait
	if waitFor <= 0 {
		waitFor = defaultPollWait
	}
	if reg.PollWaitSeconds > 0 {
		waitFor = time.Duration(reg.PollWaitSeconds) * time.Second
	}
	c.logf("docklet: fleet agent online: %s host %s (heartbeat %s, poll wait %s)", c.FleetURL, c.HostID, every, waitFor)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.heartbeatLoop(ctx, every)
	}()
	c.pollLoop(ctx, waitFor)
	cancel()
	wg.Wait()
	return nil
}

// attempt replays op with bounded exponential backoff until it succeeds or ctx
// ends. A permanently rejected request stops immediately: replaying it would
// fail identically, so the caller decides what that means.
func (c Config) attempt(ctx context.Context, what string, op func(context.Context) error) error {
	delay := backoffMin
	for {
		err := op(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if permanent(err) {
			return err
		}
		c.logf("docklet: fleet %s failed: %s (retrying in %s)", what, describe(err), delay)
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = nextBackoff(delay, false)
	}
}

// register announces this host's identity and capacity, retrying until the
// control plane answers (a host that boots before the control plane must still
// come online) or ctx ends.
func (c Config) register(ctx context.Context) (registerResponse, error) {
	body := c.registerBody()
	var out registerResponse
	err := c.attempt(ctx, "register", func(ctx context.Context) error {
		return c.registerOnce(ctx, body, &out)
	})
	if err != nil {
		return registerResponse{}, err
	}
	return out, nil
}

// registerBody snapshots this host's identity and capacity.
func (c Config) registerBody() registerRequest {
	root := c.root()
	cpuFamily := c.Family
	if cpuFamily == "" {
		cpuFamily = family(root)
	}
	body := registerRequest{
		HostID:         c.HostID,
		CPUFamily:      cpuFamily,
		Kernel:         kernelRelease(root),
		KernelRelease:  kernelRelease(root),
		Runtime:        c.Runtime,
		VcpuTotal:      vcpuTotal(root),
		MemTotalBytes:  memTotalBytes(root),
		DiskTotalBytes: diskTotalBytes(root),
	}
	if mib, known := guestMemMiB(body.MemTotalBytes>>20, c.MemoryReserveMiB); known {
		budget := mib << 20
		body.MemBudgetBytes = &budget
	}
	body.MaxDocks = maxDocks(body.VcpuTotal, body.MemTotalBytes, body.DiskTotalBytes)
	return body
}

// registerOnce registers with the held credential, or with the shared token on
// a host that has none yet. The token only claims a host that is not enrolled;
// a rejected credential is never retried with the token, because that would let
// anyone holding the token take the host over. An older control plane sends no
// credential, and the host then keeps authenticating with the shared token.
func (c Config) registerOnce(ctx context.Context, body registerRequest, out *registerResponse) error {
	auth := c.credential()
	body.AcceptsCredential = true
	err := c.fleetAs(ctx, auth.Bearer(), http.MethodPost, "/v1/fleet/register", body, out, callTimeout)
	var re *responseError
	if errors.As(err, &re) && re.status == http.StatusBadRequest {
		// An older control plane rejects the unknown field: register as before.
		body.AcceptsCredential = false
		err = c.fleetAs(ctx, auth.Bearer(), http.MethodPost, "/v1/fleet/register", body, out, callTimeout)
	}
	if err != nil {
		return err
	}
	if out.Credential != "" {
		if err := auth.Set(out.Credential); err != nil {
			c.logf("docklet: fleet credential not persisted, the host must be reset by an admin if it restarts: %v", err)
		}
	}
	return nil
}

// registerRequest and the other fleet wire types live in api/v1 so fixtures pin them.
type registerRequest = apiv1.FleetRegisterRequest

type registerResponse = apiv1.FleetRegisterResponse

type heartbeatRequest = apiv1.FleetHeartbeatRequest

func (c Config) heartbeatLoop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.heartbeat(ctx)
		}
	}
}

// heartbeat reports the live inventory. Samples are best-effort enrichment: a
// dock that cannot be sampled is left out rather than delaying the tick or
// failing the heartbeat.
func (c Config) heartbeat(ctx context.Context) {
	running := c.Host.Running()
	if running == nil {
		running = []apiv1.RunningVM{}
	}
	body := heartbeatRequest{HostID: c.HostID, UsedDocks: len(running), Runtime: c.Runtime, Running: running}
	if sampler, ok := c.Host.(Sampler); ok {
		for _, vm := range running {
			sample, err := sampler.Stats(ctx, vm.DockID)
			if err != nil {
				continue
			}
			if sample.Source == "" {
				sample.Source = "host"
			}
			body.Samples = append(body.Samples, sample)
		}
	}
	if c.Bases != nil {
		// Nil means unknown (listing failed) and is left out; empty means none held.
		if bases := c.Bases(); bases != nil {
			body.BaseFingerprints = &bases
		}
	}
	err := c.fleet(ctx, http.MethodPost, "/v1/fleet/heartbeat", body, nil, callTimeout)
	var re *responseError
	if errors.As(err, &re) && re.status == http.StatusBadRequest && body.BaseFingerprints != nil {
		// An older control plane rejects the unknown field: heartbeat as before.
		body.BaseFingerprints = nil
		err = c.fleet(ctx, http.MethodPost, "/v1/fleet/heartbeat", body, nil, callTimeout)
	}
	if err != nil && ctx.Err() == nil {
		c.logf("docklet: fleet heartbeat failed: %s", describe(err))
	}
}

// pollLoop long-polls for commands and hands each to a worker so polling never
// blocks on a running op. Poll failures back off; a rejected poll backs off to the ceiling so a
// misconfigured host cannot hot-loop the control plane.
func (c Config) pollLoop(ctx context.Context, waitFor time.Duration) {
	slots := make(chan struct{}, maxConcurrentExecs)
	terminals := make(chan struct{}, maxConcurrentTerminals)
	tunnels := make(chan struct{}, maxConcurrentTunnels)
	var running sync.WaitGroup
	defer running.Wait()
	lifecycle := newLifecycle()
	delay := backoffMin
	for ctx.Err() == nil {
		cmd, err := c.pollOnce(ctx, waitFor)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			delay = nextBackoff(delay, permanent(err))
			c.logf("docklet: fleet poll failed: %s (retrying in %s)", describe(err), delay)
			if !sleep(ctx, delay) {
				return
			}
		case cmd == nil:
			delay = backoffMin
		default:
			delay = backoffMin
			switch cmd.Op {
			case terminalOp:
				c.spawn(ctx, *cmd, terminals, &running, "too many open terminals on this host", c.openTerminal)
			case tunnelOp:
				c.spawn(ctx, *cmd, tunnels, &running, "too many open port tunnels on this host", c.openTunnel)
			default:
				c.dispatch(ctx, *cmd, slots, lifecycle, &running)
			}
		}
	}
}

// dispatch never blocks the poll loop: lifecycle ops queue per dock, everything
// else runs concurrently and a full host refuses instead of queueing.
func (c Config) dispatch(ctx context.Context, cmd command, slots chan struct{}, lc *lifecycle, running *sync.WaitGroup) {
	// The control plane redelivers a claim it thinks stale; a command still queued or running here must not run twice.
	if !lc.claim(cmd.ID) {
		c.logf("docklet: ignoring redelivered command %s (%s): still in flight", cmd.ID, cmd.Op)
		return
	}
	run := func(ctx context.Context, cmd command) {
		defer lc.finish(cmd.ID)
		c.execute(ctx, cmd)
	}
	if !lifecycleOps[cmd.Op] {
		if !c.spawn(ctx, cmd, slots, running, "too many concurrent commands on this host", run) {
			lc.finish(cmd.ID)
		}
		return
	}
	if !lc.submit(ctx, lifecycleKey(cmd), running, func() { run(ctx, cmd) }) {
		lc.finish(cmd.ID)
		c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: false, Code: "host_busy", Message: "too many lifecycle operations queued on this host"})
	}
}

// lifecycleOps mutate one dock's VM and must never overlap for that dock.
var lifecycleOps = map[string]bool{
	"provision": true, "pause": true, "snapshot.create": true, "snapshot.load": true,
	"destroy": true, "fast-run.create": true, "fast-run.terminate": true,
}

// lifecycleKey names the dock (or fast runtime) a lifecycle command targets.
func lifecycleKey(cmd command) string {
	var ids struct {
		DockID    string `json:"dockId"`
		RuntimeID string `json:"runtimeId"`
	}
	_ = json.Unmarshal(cmd.Payload, &ids)
	if ids.DockID != "" {
		return ids.DockID
	}
	if ids.RuntimeID != "" {
		return ids.RuntimeID
	}
	// An unreadable payload serializes with every other unkeyed op.
	return ""
}

// lifecycle runs queued tasks in FIFO order per key, with bounded total concurrency.
type lifecycle struct {
	mu      sync.Mutex
	queues  map[string][]func()
	pending int
	sem     chan struct{}
	// ids holds commands queued or running so a redelivery is recognised.
	ids map[string]bool
}

func newLifecycle() *lifecycle {
	return &lifecycle{queues: map[string][]func(){}, sem: make(chan struct{}, maxConcurrentLifecycle), ids: map[string]bool{}}
}

// claim marks a command ID in flight; false means it already is.
func (l *lifecycle) claim(id string) bool {
	if id == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ids[id] {
		return false
	}
	l.ids[id] = true
	return true
}

func (l *lifecycle) finish(id string) {
	l.mu.Lock()
	delete(l.ids, id)
	l.mu.Unlock()
}

// submit queues task behind earlier tasks for key; false means the host is full.
func (l *lifecycle) submit(ctx context.Context, key string, wg *sync.WaitGroup, task func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending >= maxPendingLifecycle {
		return false
	}
	l.pending++
	q, active := l.queues[key]
	l.queues[key] = append(q, task)
	if !active {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.drain(ctx, key)
		}()
	}
	return true
}

// drain runs key's tasks one at a time until its queue is empty.
func (l *lifecycle) drain(ctx context.Context, key string) {
	for {
		l.mu.Lock()
		q := l.queues[key]
		if len(q) == 0 {
			delete(l.queues, key)
			l.mu.Unlock()
			return
		}
		task := q[0]
		l.queues[key] = q[1:]
		l.mu.Unlock()
		select {
		case l.sem <- struct{}{}:
			task()
			<-l.sem
		case <-ctx.Done():
			// Shutting down: the control plane redelivers, the journal dedupes.
		}
		l.mu.Lock()
		l.pending--
		l.mu.Unlock()
	}
}

// spawn runs cmd beside the poll loop while a slot is free and refuses it otherwise.
func (c Config) spawn(ctx context.Context, cmd command, slots chan struct{}, running *sync.WaitGroup, busy string, run func(context.Context, command)) bool {
	select {
	case slots <- struct{}{}:
	default:
		c.post(ctx, cmd, resultRequest{HostID: c.HostID, Ok: false, Code: "host_busy", Message: busy})
		return false
	}
	running.Add(1)
	go func() {
		defer running.Done()
		defer func() { <-slots }()
		run(ctx, cmd)
	}()
	return true
}

// pollOnce long-polls for the next queued command; a nil command means the
// wait elapsed with nothing to do.
func (c Config) pollOnce(ctx context.Context, waitFor time.Duration) (*command, error) {
	query := url.Values{"hostId": []string{c.HostID}}
	if waitFor > 0 {
		query.Set("wait", strconv.Itoa(int(waitFor.Seconds())))
	}
	var out apiv1.FleetPollResponse
	err := c.fleet(ctx, http.MethodGet, "/v1/fleet/commands?"+query.Encode(), nil, &out, waitFor+pollGrace)
	if err != nil {
		return nil, err
	}
	return out.Command, nil
}

type command = apiv1.FleetCommand

type resultRequest = apiv1.FleetResultRequest

// execute relays one command to the local API and reports the docklet's own
// answer back to the control plane.
func (c Config) execute(ctx context.Context, cmd command) {
	c.post(ctx, cmd, c.relay(ctx, cmd))
}

// post reports one command outcome to the control plane.
func (c Config) post(ctx context.Context, cmd command, result resultRequest) {
	if len(result.Message) > maxResultMessage {
		result.Message = result.Message[:maxResultMessage-len(truncatedMarker)] + truncatedMarker
	}
	err := c.attempt(ctx, "result for command "+cmd.ID, func(ctx context.Context) error {
		return c.fleet(ctx, http.MethodPost, "/v1/fleet/commands/"+url.PathEscape(cmd.ID)+"/result", result, nil, callTimeout)
	})
	if err != nil && ctx.Err() == nil {
		// Nothing else can be done for this command: the awaiter fails it with
		// host_timeout, and the journal dedupes any redelivery.
		c.logf("docklet: fleet result for command %s gave up: %s", cmd.ID, describe(err))
	}
}

// relay replays a command against the local docklet API and returns the answer
// in the shape the control plane expects.
func (c Config) relay(ctx context.Context, cmd command) resultRequest {
	fail := func(code, message string) resultRequest {
		return resultRequest{HostID: c.HostID, Ok: false, Code: code, Message: message}
	}
	op, ok := ops[cmd.Op]
	if !ok {
		return fail("host_error", fmt.Sprintf("unknown fleet op %q", cmd.Op))
	}
	route, err := op.route(cmd.Payload)
	if err != nil {
		return fail("host_error", err.Error())
	}
	if cmd.DeadlineMs > 0 {
		// The command's own budget bounds the relay; a result past it is
		// useless to the awaiter anyway.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cmd.DeadlineMs)*time.Millisecond)
		defer cancel()
	}
	var body io.Reader
	if op.method == http.MethodPost {
		body = bytes.NewReader(cmd.Payload)
	}
	req, err := http.NewRequestWithContext(ctx, op.method, c.localBase()+route, body)
	if err != nil {
		return fail("host_error", err.Error())
	}
	if c.LocalToken != "" {
		req.Header.Set("authorization", "Bearer "+c.LocalToken)
	}
	if op.method == http.MethodPost {
		req.Header.Set("content-type", "application/json")
		req.Header.Set("idempotency-token", cmd.IdempotencyToken)
	}
	if cmd.DeadlineMs > 0 {
		req.Header.Set(deadlineHeader, strconv.FormatInt(cmd.DeadlineMs, 10))
	}
	env, status, err := c.do(req)
	if err != nil {
		code := "host_unreachable"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "host_timeout"
		}
		return fail(code, err.Error())
	}
	if status >= 400 || !env.Ok {
		code := env.Code
		if code == "" {
			code = "host_error"
		}
		message := env.Message
		if message == "" {
			message = fmt.Sprintf("docklet HTTP %d", status)
		}
		return fail(code, message)
	}
	return resultRequest{HostID: c.HostID, Ok: true, Data: env.Data}
}

// opSpec maps one fleet op onto the docklet route that implements it. route
// builds the local path for GET ops; POST ops take the payload as their body.
type opSpec struct {
	method string
	route  func(payload []byte) (string, error)
}

// getFields is the payload shape GET ops carry.
type getFields struct {
	Dock       string `json:"dock"`
	SnapshotID string `json:"snapshotId"`
	Path       string `json:"path"`
}

// constRoute is a route with no payload-derived parts.
func constRoute(path string) func([]byte) (string, error) {
	return func([]byte) (string, error) { return path, nil }
}

// getRoute builds a route from the command payload's fields.
func getRoute(build func(getFields) string) func([]byte) (string, error) {
	return func(payload []byte) (string, error) {
		var fields getFields
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &fields); err != nil {
				return "", fmt.Errorf("unreadable payload: %w", err)
			}
		}
		// Empty ids are left for the local API to refuse as missing; set ones must be safe.
		for kind, id := range map[string]string{"dock": fields.Dock, "snapshotId": fields.SnapshotID} {
			if id == "" {
				continue
			}
			if err := ids.Check(kind, id); err != nil {
				return "", err
			}
		}
		return build(fields), nil
	}
}

// ops mirrors the docklet's versioned contract: same names, same routes.
var ops = map[string]opSpec{
	"provision":          {method: http.MethodPost, route: constRoute("/v1/provision")},
	"pause":              {method: http.MethodPost, route: constRoute("/v1/pause")},
	"snapshot.create":    {method: http.MethodPost, route: constRoute("/v1/snapshots/create")},
	"snapshot.load":      {method: http.MethodPost, route: constRoute("/v1/snapshots/load")},
	"destroy":            {method: http.MethodPost, route: constRoute("/v1/destroy")},
	"fast-run.create":    {method: http.MethodPost, route: constRoute("/v1/fast-runs/create")},
	"fast-run.exec":      {method: http.MethodPost, route: constRoute("/v1/fast-runs/exec")},
	"fast-run.read":      {method: http.MethodPost, route: constRoute("/v1/fast-runs/read")},
	"fast-run.terminate": {method: http.MethodPost, route: constRoute("/v1/fast-runs/terminate")},
	"status":             {method: http.MethodGet, route: constRoute("/v1/status")},
	"stats": {method: http.MethodGet, route: getRoute(func(f getFields) string {
		return "/v1/stats?dock=" + url.QueryEscape(f.Dock)
	})},
	"generation.tree": {method: http.MethodGet, route: getRoute(func(f getFields) string {
		return "/v1/generations/" + url.PathEscape(f.SnapshotID) + "/tree"
	})},
	"generation.file": {method: http.MethodGet, route: getRoute(func(f getFields) string {
		return "/v1/generations/" + url.PathEscape(f.SnapshotID) + "/files?path=" + url.QueryEscape(f.Path)
	})},
}

// envelope is the shape the control plane and the local docklet API both use.
// Data stays raw so relayed results are forwarded verbatim.
type envelope struct {
	Ok      bool            `json:"ok"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// fleet performs one authenticated control-plane call and decodes the
// envelope's data into out.
func (c Config) fleet(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	used := c.bearer()
	err := c.fleetAs(ctx, used, method, path, body, out, timeout)
	var re *responseError
	if !errors.As(err, &re) || re.status != http.StatusUnauthorized || c.Auth == nil {
		return err
	}
	// Registered before the backend issued credentials: claim one now with the
	// shared token (the backend only issues to a host that has none).
	claimed := c.Auth.Claim(used, func() error {
		c.logf("docklet: fleet token rejected without a host credential, claiming one")
		var out registerResponse
		err := c.registerOnce(ctx, c.registerBody(), &out)
		if err != nil {
			c.logf("docklet: claiming a host credential failed: %s", describe(err))
		}
		return err
	})
	if !claimed {
		return err
	}
	return c.fleetAs(ctx, c.bearer(), method, path, body, out, timeout)
}

// credential is the configured credential, or an in-memory one seeded from Token.
func (c Config) credential() *fleetauth.Credential {
	if c.Auth != nil {
		return c.Auth
	}
	return fleetauth.Open("", c.Token)
}

// bearer is the token to present now: the per-host credential once issued.
func (c Config) bearer() string {
	if c.Auth != nil {
		return c.Auth.Bearer()
	}
	return c.Token
}

// fleetAs performs one control-plane call presenting the given bearer.
func (c Config) fleetAs(ctx context.Context, bearer, method, path string, body, out any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.FleetURL+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	env, status, err := c.do(req)
	if err != nil {
		return err
	}
	if status >= 400 || !env.Ok {
		return &responseError{status: status, code: env.Code, message: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// do performs one request and decodes its envelope. A body that is not an
// envelope (a bare mux 404, a proxy error page) still reports its status.
func (c Config) do(req *http.Request) (envelope, int, error) {
	resp, err := c.client().Do(req)
	if err != nil {
		return envelope{}, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return envelope{}, resp.StatusCode, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return envelope{Message: fmt.Sprintf("unexpected HTTP %d response", resp.StatusCode)}, resp.StatusCode, nil
	}
	return env, resp.StatusCode, nil
}

func (c Config) client() Doer {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c Config) localBase() string {
	if c.LocalAPIBase != "" {
		return strings.TrimRight(c.LocalAPIBase, "/")
	}
	return DefaultLocalAPIBase
}

func (c Config) root() string {
	if c.Root != "" {
		return c.Root
	}
	return "/"
}

func (c Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// responseError reports a response that did not carry a successful envelope.
type responseError struct {
	status  int
	code    string
	message string
}

func (e *responseError) Error() string {
	detail := e.message
	if e.code != "" {
		if detail == "" {
			detail = e.code
		} else {
			detail = e.code + ": " + detail
		}
	}
	if e.status > 0 {
		if detail == "" {
			return fmt.Sprintf("HTTP %d", e.status)
		}
		return fmt.Sprintf("HTTP %d: %s", e.status, detail)
	}
	if detail == "" {
		return "request rejected"
	}
	return detail
}

// permanent reports whether replaying this request would fail identically:
// every 4xx except 401 (the token may be rotated) and 429 (back off and retry).
func (e *responseError) permanent() bool {
	return e.status >= 400 && e.status < 500 && e.status != http.StatusUnauthorized && e.status != http.StatusTooManyRequests
}

func permanent(err error) bool {
	var re *responseError
	return errors.As(err, &re) && re.permanent()
}

// describe renders a failed call for the operator, calling out the token case
// so a 401 is never mistaken for a transient outage.
func describe(err error) string {
	var re *responseError
	if errors.As(err, &re) && re.status == http.StatusUnauthorized {
		return "unauthorized: the host credential was revoked, or the fleet token is wrong; for a revoked credential an instance admin must reset it (POST /v1/hosts/<id>/reset-credential), then install the new one in the credential file and restart the docklet (" + err.Error() + ")"
	}
	return err.Error()
}

// sleep waits for d, reporting false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextBackoff doubles d up to the ceiling; a permanently rejected request
// jumps straight to the ceiling so a rejected caller can never hot-loop.
func nextBackoff(d time.Duration, permanent bool) time.Duration {
	if permanent {
		return backoffMax
	}
	if d < backoffMin {
		d = backoffMin
	}
	if d *= 2; d > backoffMax {
		return backoffMax
	}
	return d
}

// family reads the CPU family that snapshot placement keys on.
func family(root string) string {
	current, err := cpufamily.Current(filepath.Join(root, "proc/cpuinfo"))
	if err != nil || current == "" {
		return "unknown"
	}
	return current
}

// vcpuTotal counts the logical CPUs reported by /proc/cpuinfo.
func vcpuTotal(root string) int {
	raw, err := os.ReadFile(filepath.Join(root, "proc/cpuinfo"))
	if err != nil {
		return 0
	}
	total := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "processor") && strings.Contains(line, ":") {
			total++
		}
	}
	return total
}

// memTotalBytes reads physical memory from /proc/meminfo.
func memTotalBytes(root string) int64 {
	raw, err := os.ReadFile(filepath.Join(root, "proc/meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		var kb int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, "MemTotal:"), "%d kB", &kb); err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// kernelRelease reads the running kernel release.
func kernelRelease(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, "proc/sys/kernel/osrelease"))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(raw))
}

// diskTotalBytes reports the capacity of the filesystem backing the host.
func diskTotalBytes(root string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return 0
	}
	return int64(stat.Blocks) * int64(stat.Bsize)
}

// FreeDiskBytes reports the space available to unprivileged writes on the filesystem holding path; 0 when unknown.
func FreeDiskBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}

// Default dock size the reported slot count assumes; placement itself admits
// by each dock's requested resources.
const (
	defaultCPUs      = 2
	defaultMemoryMiB = 4096
	defaultDiskGiB   = 20
)

// maxDocks budgets dock slots at the default size, so the reported capacity
// never overcommits a known resource. Unknown totals do not constrain the budget.
func maxDocks(vcpu int, memBytes, diskBytes int64) int {
	slots := 0
	for _, c := range []struct{ total, per int64 }{
		{int64(vcpu), defaultCPUs},
		{memBytes, defaultMemoryMiB << 20},
		{diskBytes, defaultDiskGiB << 30},
	} {
		if c.total <= 0 {
			continue
		}
		if n := int(max(c.total/c.per, 1)); slots == 0 || n < slots {
			slots = n
		}
	}
	return max(slots, 1)
}

// guestMemMiB is the memory guests may commit: total minus the host reserve (default the larger of 1 GiB and 10%).
// An unknown total reports unknown; a reserve at or above RAM reports a known zero, meaning no guest memory.
func guestMemMiB(totalMiB, reserveMiB int64) (mib int64, known bool) {
	if totalMiB <= 0 {
		return 0, false
	}
	if reserveMiB <= 0 {
		reserveMiB = max(1024, totalMiB/10)
	}
	return max(totalMiB-reserveMiB, 0), true
}

// Capacity reports the host's logical CPUs and guest-committable memory in MiB for the admission budget;
// 0 means unknown and -1 means the reserve consumed all memory, so every boot is refused.
func Capacity(root string, memoryReserveMiB int64) (vcpu, memMiB int64) {
	mib, known := guestMemMiB(memTotalBytes(root)>>20, memoryReserveMiB)
	if known && mib == 0 {
		mib = -1
	}
	return int64(vcpuTotal(root)), mib
}

// TotalDiskBytes reports the capacity of the filesystem holding path; 0 when unknown.
func TotalDiskBytes(path string) int64 { return diskTotalBytes(path) }
