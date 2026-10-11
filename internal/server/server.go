// Package server translates the versioned docklet HTTP contract onto the
// host orchestrator. Mutating routes replay byte-identical responses for
// retried idempotency tokens and refuse work while the host is unready.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/fc"
	"github.com/usedock/docklet/internal/host"
	"github.com/usedock/docklet/internal/idem"
	"github.com/usedock/docklet/internal/ids"
	"github.com/usedock/docklet/internal/snapmgr"
)

// Deps wires one server; nil Host or Journal degrades to loud stubs.
type Deps struct {
	Host    *host.Host
	Journal *idem.Journal
	Ready   func() []string
	HostID  string
	// Token is the bearer token every route except health and readiness requires.
	Token string
}

// Server serves the docklet routes.
type Server struct {
	deps Deps
	mux  *http.ServeMux

	mu       sync.Mutex
	snapping map[string]bool
	// docks serializes provision/pause/load/destroy per dock until the detached work really ends.
	docks map[string]*dockLock
	// inflight holds running tokens in memory; once a run ends the journal is the only record, and failed runs are not journaled.
	inflight map[string]*flight
}

// tryAcquire serializes snapshot captures per dock; a second concurrent
// capture fails fast instead of wedging the VM or piling uploads.
func (s *Server) tryAcquire(dockID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapping == nil {
		s.snapping = map[string]bool{}
	}
	if s.snapping[dockID] {
		return false
	}
	s.snapping[dockID] = true
	return true
}

func (s *Server) release(dockID string) {
	s.mu.Lock()
	delete(s.snapping, dockID)
	s.mu.Unlock()
}

// dockBusyWait caps how long a lifecycle op waits behind an earlier one for the same dock.
var dockBusyWait = 30 * time.Second

// dockLock is one held per-dock lock; token names the command holding it.
type dockLock struct {
	done  chan struct{}
	token string
}

// lockWait is dockBusyWait, shortened so a busy answer lands before the command's own deadline.
func lockWait(r *http.Request) time.Duration {
	if ms, err := strconv.ParseInt(r.Header.Get(DeadlineHeader), 10, 64); err == nil && ms > 0 {
		if d := time.Duration(ms)*time.Millisecond - 2*time.Second; d < dockBusyWait {
			return max(d, 0)
		}
	}
	return dockBusyWait
}

// lockToken is the command's dedupe token, matching idempotentMode.
func lockToken(r *http.Request, token string) string {
	if token == "" {
		return r.Header.Get("idempotency-token")
	}
	return token
}

// errDockBusy marks a lifecycle op that could not get the per-dock lock in time.
var errDockBusy = errors.New("another lifecycle operation is still running for this dock")

// lockDock waits (bounded) for the dock's lifecycle lock; call the returned func when the work ends.
func (s *Server) lockDock(dockID, token string, wait time.Duration) (func(), error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if s.docks == nil {
			s.docks = map[string]*dockLock{}
		}
		cur, held := s.docks[dockID]
		if !held {
			mine := &dockLock{done: make(chan struct{}), token: token}
			s.docks[dockID] = mine
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				delete(s.docks, dockID)
				s.mu.Unlock()
				close(mine.done)
			}, nil
		}
		s.mu.Unlock()
		select {
		case <-cur.done:
		case <-deadline.C:
			return nil, errDockBusy
		}
	}
}

// NewMux returns the route table for deps.
func NewMux(d Deps) *http.ServeMux {
	s := &Server{deps: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("/v1/healthz", handleHealth)
	s.mux.HandleFunc("/v1/readiness", s.handleReadiness)
	s.mux.HandleFunc("/v1/status", s.auth(s.handleStatus))
	s.mux.HandleFunc("/v1/stats", s.auth(s.handleStats))
	s.mux.HandleFunc("/v1/provision", s.auth(s.guard(s.handleProvision)))
	s.mux.HandleFunc("/v1/pause", s.auth(s.guard(s.handlePause)))
	s.mux.HandleFunc("/v1/snapshots/create", s.auth(s.guard(s.handleSnapshotCreate)))
	s.mux.HandleFunc("/v1/snapshots/load", s.auth(s.guard(s.handleSnapshotLoad)))
	s.mux.HandleFunc("/v1/destroy", s.auth(s.guard(s.handleDestroy)))
	s.mux.HandleFunc("/v1/fast-runs/create", s.auth(s.guard(s.handleFastRunCreate)))
	s.mux.HandleFunc("/v1/fast-runs/exec", s.auth(s.guard(s.handleFastRunExec)))
	s.mux.HandleFunc("/v1/fast-runs/read", s.auth(s.guard(s.handleFastRunRead)))
	s.mux.HandleFunc("/v1/fast-runs/terminate", s.auth(s.guard(s.handleFastRunTerminate)))
	s.mux.HandleFunc("/v1/heartbeat", s.auth(s.handleHeartbeat))
	s.mux.HandleFunc("/v1/generations/{id}/tree", s.auth(s.handleGenerationTree))
	s.mux.HandleFunc("/v1/generations/{id}/files", s.auth(s.handleGenerationFile))
	return s.mux
}

func writeJSON(w http.ResponseWriter, status int, env apiv1.Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: map[string]string{"version": apiv1.APIVersion}})
}

func (s *Server) ready() []string {
	if s.deps.Ready == nil {
		return nil
	}
	return s.deps.Ready()
}

// guard fails mutating calls closed while the host is unready.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reasons := s.ready(); len(reasons) > 0 {
			writeJSON(w, http.StatusServiceUnavailable, apiv1.Envelope{Ok: false, Code: "host_not_ready", Message: "host not ready: " + reasons[0]})
			return
		}
		if s.deps.Host == nil {
			writeJSON(w, http.StatusNotImplemented, apiv1.Envelope{Ok: false, Code: "not_implemented", Message: "no host driver configured"})
			return
		}
		next(w, r)
	}
}

// idempotent replays the recorded response for a seen token.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, token string, fn func() (any, error)) {
	s.idempotentMode(w, r, token, true, fn)
}

// idempotentTransient dedupes by token but never persists the result, for
// calls whose output is guest file content (fast-run read); exec replays within the size cap.
func (s *Server) idempotentTransient(w http.ResponseWriter, r *http.Request, token string, fn func() (any, error)) {
	s.idempotentMode(w, r, token, false, fn)
}

const (
	// DeadlineHeader carries the command's budget in ms; the wait ends a margin after it.
	DeadlineHeader = "X-Deadline-Ms"
	deadlineMargin = 10 * time.Second
	// maxWait caps a wait when the caller sent no deadline.
	maxWait = 30 * time.Minute
)

// flight is one running token; waiters share its outcome once done closes.
type flight struct {
	done   chan struct{}
	status int
	body   []byte
}

// workCeiling bounds how long a token stays in flight: 3x the command deadline, or maxWait without one.
func workCeiling(r *http.Request) time.Duration {
	if ms, err := strconv.ParseInt(r.Header.Get(DeadlineHeader), 10, 64); err == nil && ms > 0 {
		return 3 * time.Duration(ms) * time.Millisecond
	}
	return maxWait
}

// waitCtx bounds how long a handler waits, never how long the work itself runs.
func waitCtx(r *http.Request) (context.Context, context.CancelFunc) {
	limit := maxWait
	if ms, err := strconv.ParseInt(r.Header.Get(DeadlineHeader), 10, 64); err == nil && ms > 0 {
		limit = time.Duration(ms)*time.Millisecond + deadlineMargin
	}
	return context.WithTimeout(r.Context(), limit)
}

func (s *Server) idempotentMode(w http.ResponseWriter, r *http.Request, token string, retain bool, fn func() (any, error)) {
	// Without a body token the agent's header still keys dedupe, so token-less ops get the same protection.
	if token == "" {
		token = r.Header.Get("idempotency-token")
	}
	waitCtx, cancel := waitCtx(r)
	defer cancel()
	f := s.begin(token, retain, workCeiling(r), fn)
	if f == nil {
		cached, _ := s.deps.Journal.Lookup(token)
		if cached.Marker != nil {
			// A 4xx envelope is relayed to the control plane as a failed result with this code.
			writeJSON(w, http.StatusConflict, apiv1.Envelope{Ok: false, Code: "result_not_retained", Message: "already executed, result not retained"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mustEnvelope(cached.Response))
		return
	}
	select {
	case <-f.done:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write(f.body)
	case <-waitCtx.Done():
		// The work keeps running detached; a retry of this token waits on it again.
		writeJSON(w, http.StatusConflict, apiv1.Envelope{Ok: false, Code: "in_progress", Message: "command still running on this host"})
	}
}

// begin returns the running flight for token, starting one when none exists;
// nil means the journal already holds the token's result.
func (s *Server) begin(token string, retain bool, ceiling time.Duration, fn func() (any, error)) *flight {
	if token == "" || s.deps.Journal == nil {
		return s.start("", false, retain, ceiling, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.inflight[token]; ok {
		return f
	}
	if _, ok := s.deps.Journal.Lookup(token); ok {
		return nil
	}
	return s.startLocked(token, true, retain, ceiling, fn)
}

func (s *Server) start(token string, track, retain bool, ceiling time.Duration, fn func() (any, error)) *flight {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(token, track, retain, ceiling, fn)
}

// startLocked runs fn detached from the request so a VM started inside it
// outlives the handler; the journal is written before the token leaves flight.
func (s *Server) startLocked(token string, track, retain bool, ceiling time.Duration, fn func() (any, error)) *flight {
	f := &flight{done: make(chan struct{})}
	if track {
		if s.inflight == nil {
			s.inflight = map[string]*flight{}
		}
		s.inflight[token] = f
	}
	type outcome struct {
		status int
		body   []byte
	}
	result := make(chan outcome, 1)
	go func() {
		status, body := s.run(token, retain, fn)
		result <- outcome{status, body}
	}()
	go func() {
		defer close(f.done)
		timer := time.NewTimer(ceiling)
		defer timer.Stop()
		select {
		case o := <-result:
			f.status, f.body = o.status, o.body
		case <-timer.C:
			// The work cannot be killed (it may own a VM); a late success still journals, but the token stops blocking retries.
			log.Printf("docklet: command %q exceeded its %s ceiling, releasing its in-flight entry", token, ceiling)
			s.logHeldDocks(token)
			f.status, f.body = http.StatusGatewayTimeout, mustMarshal(apiv1.Envelope{Ok: false, Code: "host_timeout", Message: "command exceeded its time ceiling on this host"})
		}
		if track {
			s.mu.Lock()
			if s.inflight[token] == f {
				delete(s.inflight, token)
			}
			s.mu.Unlock()
		}
	}()
	return f
}

// logHeldDocks makes a dock still locked by a ceiling-expired command visible to operators.
func (s *Server) logHeldDocks(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for dock, l := range s.docks {
		if l.token == token {
			log.Printf("docklet: dock %q stays locked: command %q is still running past its ceiling", dock, token)
		}
	}
}

func (s *Server) run(token string, retain bool, fn func() (any, error)) (status int, body []byte) {
	defer func() {
		if p := recover(); p != nil {
			status, body = http.StatusInternalServerError, mustMarshal(apiv1.Envelope{Ok: false, Code: "host_error", Message: fmt.Sprint("panic: ", p)})
		}
	}()
	data, err := fn()
	if err != nil {
		return statusFor(err), mustMarshal(apiv1.Envelope{Ok: false, Code: codeFor(err), Message: err.Error()})
	}
	if s.deps.Journal != nil && token != "" {
		raw, _ := json.Marshal(data)
		if retain {
			_ = s.deps.Journal.Record(token, raw)
		} else {
			_ = s.deps.Journal.RecordMarker(token, raw)
		}
	}
	return http.StatusOK, mustMarshal(apiv1.Envelope{Ok: true, Data: data})
}

func mustMarshal(env apiv1.Envelope) []byte {
	raw, _ := json.Marshal(env)
	return append(raw, '\n')
}

func mustEnvelope(data json.RawMessage) []byte {
	raw, _ := json.Marshal(apiv1.Envelope{Ok: true, Data: json.RawMessage(data)})
	return raw
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	reasons := s.ready()
	if len(reasons) > 0 {
		writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: apiv1.Readiness{Ready: false, Reasons: reasons}})
		return
	}
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: apiv1.Readiness{Ready: len(reasons) == 0}})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	running := []apiv1.RunningVM{}
	if s.deps.Host != nil {
		running = s.deps.Host.Running()
	}
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: map[string]any{"hostId": s.deps.HostID, "running": running}})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.deps.Host == nil {
		writeJSON(w, http.StatusNotImplemented, apiv1.Envelope{Ok: false, Code: "not_implemented", Message: "no host driver configured"})
		return
	}
	if badID(w, "dock", r.URL.Query().Get("dock"), false) {
		return
	}
	sample, err := s.deps.Host.Stats(lifecycleCtx(r), r.URL.Query().Get("dock"))
	if err != nil {
		writeJSON(w, statusFor(err), apiv1.Envelope{Ok: false, Code: codeFor(err), Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: sample})
}

// lifecycleCtx detaches VM lifetime from the HTTP request: the SDK
// SIGTERMs the microVM when the context passed to Start is cancelled,
// and request contexts cancel as soon as the response is written.
func lifecycleCtx(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ProvisionRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "unreadable provision body"})
		return
	}
	if badID(w, "dockId", req.DockID, false) || badID(w, "snapshotId", req.SnapshotID, true) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		unlock, err := s.lockDock(req.DockID, lockToken(r, req.IdempotencyToken), lockWait(r))
		if err != nil {
			return nil, err
		}
		defer unlock()
		var ip string
		if req.ExecAgent {
			ip, err = s.deps.Host.ProvisionWithAgent(lifecycleCtx(r), req.DockID, req.Resources, req.Image, req.SnapshotID, req.EgressAllow)
		} else if len(req.EgressAllow) > 0 {
			err = errors.New("egressAllow requires execAgent")
		} else {
			ip, err = s.deps.Host.Provision(lifecycleCtx(r), req.DockID, req.Resources, req.Image, req.SnapshotID)
		}
		if err != nil {
			return nil, err
		}
		return apiv1.ProvisionResponse{IP: ip, HostID: s.deps.HostID}, nil
	})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req apiv1.PauseRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "unreadable pause body"})
		return
	}
	if badID(w, "dockId", req.DockID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		unlock, err := s.lockDock(req.DockID, lockToken(r, req.IdempotencyToken), lockWait(r))
		if err != nil {
			return nil, err
		}
		defer unlock()
		return map[string]string{"dockId": req.DockID}, s.deps.Host.Pause(lifecycleCtx(r), req.DockID)
	})
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	var req apiv1.SnapshotCreateRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "unreadable snapshot body"})
		return
	}
	parent := ""
	if req.ParentID != nil {
		parent = *req.ParentID
	}
	if badID(w, "dockId", req.DockID, false) || badID(w, "parentId", parent, true) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		// Held inside the detached work so a timed-out handler cannot free the dock early.
		if !s.tryAcquire(req.DockID) {
			return nil, errSnapshotBusy
		}
		defer s.release(req.DockID)
		unlock, err := s.lockDock(req.DockID, lockToken(r, req.IdempotencyToken), lockWait(r))
		if err != nil {
			return nil, err
		}
		defer unlock()
		id, size, family, err := s.deps.Host.SnapshotCreate(lifecycleCtx(r), req.DockID, req.ParentID, req.Kind, req.ResumeAfter, req.Memory)
		if err != nil {
			return nil, err
		}
		return apiv1.SnapshotCreateResponse{SnapshotID: id, SizeBytes: size, CPUFamily: family, BaseFingerprint: s.deps.Host.GenerationBase(lifecycleCtx(r), id)}, nil
	})
}

func (s *Server) handleSnapshotLoad(w http.ResponseWriter, r *http.Request) {
	var req apiv1.SnapshotLoadRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "unreadable load body"})
		return
	}
	if badID(w, "dockId", req.DockID, false) || badID(w, "snapshotId", req.SnapshotID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		unlock, err := s.lockDock(req.DockID, lockToken(r, req.IdempotencyToken), lockWait(r))
		if err != nil {
			return nil, err
		}
		defer unlock()
		if len(req.EgressAllow) > 0 && !req.ExecAgent {
			return nil, errors.New("egressAllow requires execAgent")
		}
		var ip string
		if req.ExecAgent {
			ip, err = s.deps.Host.SnapshotLoadWithAgent(lifecycleCtx(r), req.DockID, req.Resources, req.SnapshotID, req.EgressAllow)
		} else {
			ip, err = s.deps.Host.SnapshotLoad(lifecycleCtx(r), req.DockID, req.Resources, req.SnapshotID)
		}
		if err != nil {
			return nil, err
		}
		return apiv1.SnapshotLoadResponse{IP: ip, HostID: s.deps.HostID}, nil
	})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	var req apiv1.DestroyRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "unreadable destroy body"})
		return
	}
	if badID(w, "dockId", req.DockID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		unlock, err := s.lockDock(req.DockID, lockToken(r, req.IdempotencyToken), lockWait(r))
		if err != nil {
			return nil, err
		}
		defer unlock()
		return map[string]string{"dockId": req.DockID}, s.deps.Host.Destroy(lifecycleCtx(r), req.DockID)
	})
}

func (s *Server) handleGenerationTree(w http.ResponseWriter, r *http.Request) {
	if s.deps.Host == nil {
		writeJSON(w, http.StatusNotImplemented, apiv1.Envelope{Ok: false, Code: "not_implemented", Message: "no host driver configured"})
		return
	}
	if badID(w, "snapshotId", r.PathValue("id"), false) {
		return
	}
	entries, err := s.deps.Host.GenerationTree(lifecycleCtx(r), r.PathValue("id"))
	if err != nil {
		writeJSON(w, statusFor(err), apiv1.Envelope{Ok: false, Code: codeFor(err), Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: map[string]any{"snapshotId": r.PathValue("id"), "files": entries}})
}

// maxFileResponseBytes keeps the JSON-encoded file reply under the agent's 8 MiB relay cap.
const maxFileResponseBytes = 7 << 20

func encodeFileReply(raw []byte) ([]byte, error) {
	body := mustMarshal(apiv1.Envelope{Ok: true, Data: map[string]any{"content": string(raw), "size": len(raw)}})
	if len(body) > maxFileResponseBytes {
		return nil, fmt.Errorf("%w: encoded response exceeds %d bytes", disk.ErrTooLarge, maxFileResponseBytes)
	}
	return body, nil
}

func (s *Server) handleGenerationFile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Host == nil {
		writeJSON(w, http.StatusNotImplemented, apiv1.Envelope{Ok: false, Code: "not_implemented", Message: "no host driver configured"})
		return
	}
	if badID(w, "snapshotId", r.PathValue("id"), false) {
		return
	}
	raw, err := s.deps.Host.GenerationFile(lifecycleCtx(r), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, statusFor(err), apiv1.Envelope{Ok: false, Code: codeFor(err), Message: err.Error()})
		return
	}
	body, err := encodeFileReply(raw)
	if err != nil {
		writeJSON(w, statusFor(err), apiv1.Envelope{Ok: false, Code: codeFor(err), Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apiv1.Envelope{Ok: true, Data: map[string]string{"hostId": s.deps.HostID}})
}

func fastRunCapabilities() apiv1.FastRunCapabilities {
	return apiv1.FastRunCapabilities{
		Version: 1,
		Target:  "fast",
		Profile: "warm-microvm",
		Capabilities: map[string]bool{
			"workspaceIsolation": true,
			"docker":             true,
			"browser":            true,
			"systemd":            true,
			"sshTakeover":        false,
			"persistentState":    false,
			"networkPolicy":      true,
			"credentialBroker":   true,
		},
	}
}

func (s *Server) handleFastRunCreate(w http.ResponseWriter, r *http.Request) {
	var req apiv1.FastRunCreateRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: err.Error()})
		return
	}
	if req.RuntimeID == "" || req.IdempotencyToken == "" || req.GuestToken == "" {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "runtimeId, guestToken, and idempotencyToken are required"})
		return
	}
	if badID(w, "runtimeId", req.RuntimeID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		ip, err := s.deps.Host.ProvisionFast(lifecycleCtx(r), req.RuntimeID, req.Resources, req.Image, req.GuestToken, "/home/user/work", req.EgressAllow)
		if err != nil {
			return nil, err
		}
		capabilities := fastRunCapabilities()
		return apiv1.FastRunCreateResponse{RuntimeID: req.RuntimeID, IP: ip, HostID: s.deps.HostID, Capabilities: capabilities}, nil
	})
}

func (s *Server) handleFastRunExec(w http.ResponseWriter, r *http.Request) {
	var req apiv1.FastRunExecRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: err.Error()})
		return
	}
	if req.RuntimeID == "" || req.IdempotencyToken == "" || req.Script == "" {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "runtimeId, script, and idempotencyToken are required"})
		return
	}
	if badID(w, "runtimeId", req.RuntimeID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		timeout := time.Duration(req.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		return s.deps.Host.FastExec(lifecycleCtx(r), req.RuntimeID, req.Script, req.Env, timeout)
	})
}

func (s *Server) handleFastRunRead(w http.ResponseWriter, r *http.Request) {
	var req apiv1.FastRunReadRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: err.Error()})
		return
	}
	if req.RuntimeID == "" || req.Path == "" || req.IdempotencyToken == "" {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "runtimeId, path, and idempotencyToken are required"})
		return
	}
	if badID(w, "runtimeId", req.RuntimeID, false) {
		return
	}
	s.idempotentTransient(w, r, req.IdempotencyToken, func() (any, error) {
		return s.deps.Host.FastRead(lifecycleCtx(r), req.RuntimeID, req.Path)
	})
}

func (s *Server) handleFastRunTerminate(w http.ResponseWriter, r *http.Request) {
	var req apiv1.FastRunTerminateRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: err.Error()})
		return
	}
	if req.RuntimeID == "" || req.IdempotencyToken == "" {
		writeJSON(w, http.StatusBadRequest, apiv1.Envelope{Ok: false, Code: "validation_error", Message: "runtimeId and idempotencyToken are required"})
		return
	}
	if badID(w, "runtimeId", req.RuntimeID, false) {
		return
	}
	s.idempotent(w, r, req.IdempotencyToken, func() (any, error) {
		return map[string]string{"runtimeId": req.RuntimeID}, s.deps.Host.Destroy(lifecycleCtx(r), req.RuntimeID)
	})
}

// errSnapshotBusy marks a second concurrent capture for one dock.
var errSnapshotBusy = errors.New("another capture is running for this dock")

// statusFor maps domain errors to HTTP statuses.
func statusFor(err error) int {
	if errors.Is(err, errSnapshotBusy) {
		return http.StatusConflict
	}
	if errors.Is(err, errDockBusy) {
		return http.StatusConflict
	}
	if errors.Is(err, ids.ErrInvalid) {
		return http.StatusBadRequest
	}
	if _, ok := err.(*snapmgr.ErrIncompatible); ok {
		return http.StatusConflict
	}
	if errors.Is(err, disk.ErrBaseUnavailable) {
		return http.StatusConflict
	}
	if errors.Is(err, host.ErrLatestRefused) || errors.Is(err, host.ErrImageUnavailable) || errors.Is(err, fc.ErrInvalidResources) {
		return http.StatusBadRequest
	}
	if errors.Is(err, host.ErrCapacityExhausted) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, host.ErrExecUnavailable) {
		return http.StatusConflict
	}
	if errors.Is(err, disk.ErrNotRegular) {
		return http.StatusUnprocessableEntity
	}
	if errors.Is(err, disk.ErrTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusInternalServerError
}

// codeFor maps domain errors to stable snake_case codes.
func codeFor(err error) string {
	if errors.Is(err, errSnapshotBusy) {
		return "snapshot_in_progress"
	}
	if errors.Is(err, errDockBusy) {
		return "dock_busy"
	}
	if errors.Is(err, ids.ErrInvalid) {
		return "validation_error"
	}
	if _, ok := err.(*snapmgr.ErrIncompatible); ok {
		return "cpu_family_mismatch"
	}
	if errors.Is(err, disk.ErrBaseUnavailable) {
		return "base_image_unavailable"
	}
	if errors.Is(err, host.ErrImageUnavailable) {
		return "image_unavailable"
	}
	if errors.Is(err, host.ErrLatestRefused) || errors.Is(err, fc.ErrInvalidResources) {
		return "validation_error"
	}
	if errors.Is(err, host.ErrCapacityExhausted) {
		return "capacity_exhausted"
	}
	if errors.Is(err, host.ErrExecUnavailable) {
		return "exec_unavailable"
	}
	if errors.Is(err, disk.ErrNotRegular) {
		return "not_a_regular_file"
	}
	if errors.Is(err, disk.ErrTooLarge) {
		return "file_too_large"
	}
	return "host_error"
}
