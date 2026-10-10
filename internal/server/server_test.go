package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/host"
	"github.com/usedock/docklet/internal/idem"
)

const testToken = "test-local-token"

func get(t *testing.T, mux *http.ServeMux, path string) (int, apiv1.Envelope) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var env apiv1.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return rec.Code, env
}

func TestHealth(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1"})
	code, env := get(t, mux, "/v1/healthz")
	if code != http.StatusOK || !env.Ok {
		t.Fatalf("health = %d %+v, want 200 ok", code, env)
	}
}

func TestUnconfiguredFailsLoudly(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1", Token: testToken})
	for _, path := range []string{"/v1/provision", "/v1/pause", "/v1/snapshots/create", "/v1/snapshots/load", "/v1/destroy", "/v1/fast-runs/create", "/v1/fast-runs/exec", "/v1/fast-runs/read", "/v1/fast-runs/terminate"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("POST %s = %d, want 501", path, rec.Code)
		}
	}
}

func TestGuardFailsClosed(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1", Token: testToken, Ready: func() []string { return []string{"no-kvm"} }})
	req := httptest.NewRequest(http.MethodPost, "/v1/provision", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var env apiv1.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || env.Code != "host_not_ready" {
		t.Fatalf("guard = %d %+v, want 503 host_not_ready", rec.Code, env)
	}
	code, ready := get(t, mux, "/v1/readiness")
	if code != http.StatusOK || !ready.Ok {
		t.Fatalf("readiness = %d %+v", code, ready)
	}
}

func TestSnapshotMutexSerializesPerDock(t *testing.T) {
	s := &Server{}
	if !s.tryAcquire("d1") {
		t.Fatal("first acquire must succeed")
	}
	if s.tryAcquire("d1") {
		t.Fatal("second concurrent acquire must fail")
	}
	if !s.tryAcquire("d2") {
		t.Fatal("other docks must not block")
	}
	s.release("d1")
	if !s.tryAcquire("d1") {
		t.Fatal("release must free the dock")
	}
}

func TestStatus(t *testing.T) {
	mux := NewMux(Deps{HostID: "h9", Token: testToken})
	code, env := get(t, mux, "/v1/status")
	if code != http.StatusOK || !env.Ok {
		t.Fatalf("status = %d %+v", code, env)
	}
}

func TestExecUnavailableMapsToConflict(t *testing.T) {
	err := fmt.Errorf("%w: fast runtime d1 not found", host.ErrExecUnavailable)
	if statusFor(err) != http.StatusConflict || codeFor(err) != "exec_unavailable" {
		t.Fatalf("status=%d code=%s", statusFor(err), codeFor(err))
	}
	if codeFor(errors.New("boom")) != "host_error" {
		t.Fatal("other errors stay host_error")
	}
}

func TestMissingBaseMapsToBaseImageUnavailable(t *testing.T) {
	err := fmt.Errorf("generation s1: %w: base image abc is not on this host", disk.ErrBaseUnavailable)
	if statusFor(err) != http.StatusConflict || codeFor(err) != "base_image_unavailable" {
		t.Fatalf("status=%d code=%s", statusFor(err), codeFor(err))
	}
}

func TestStatusForSnapshotFileErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", disk.ErrNotRegular), http.StatusUnprocessableEntity, "not_a_regular_file"},
		{fmt.Errorf("x: %w", disk.ErrTooLarge), http.StatusRequestEntityTooLarge, "file_too_large"},
		{errors.New("boom"), http.StatusInternalServerError, "host_error"},
	}
	for _, c := range cases {
		if got := statusFor(c.err); got != c.status {
			t.Fatalf("%v status = %d", c.err, got)
		}
		if got := codeFor(c.err); got != c.code {
			t.Fatalf("%v code = %s", c.err, got)
		}
	}
}

func TestLatestRefMapsToBadRequest(t *testing.T) {
	err := fmt.Errorf("load: %w", host.ErrLatestRefused)
	if statusFor(err) != http.StatusBadRequest || codeFor(err) != "validation_error" {
		t.Fatalf("status=%d code=%s", statusFor(err), codeFor(err))
	}
}

func TestTransientResultNotJournaled(t *testing.T) {
	j, err := idem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{deps: Deps{Journal: j}}
	runs := 0
	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.idempotentTransient(rec, httptest.NewRequest(http.MethodPost, "/", nil), "tok", func() (any, error) {
			runs++
			return map[string]string{"stdout": "SECRET"}, nil
		})
		return rec
	}
	if first := call(); first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "SECRET") {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	second := call()
	var env apiv1.Envelope
	_ = json.Unmarshal(second.Body.Bytes(), &env)
	if runs != 1 || second.Code != http.StatusConflict || env.Ok || env.Code != "result_not_retained" || strings.Contains(second.Body.String(), "SECRET") {
		t.Fatalf("replay runs=%d %d %s", runs, second.Code, second.Body)
	}
}

func newTokenServer(t *testing.T) *Server {
	t.Helper()
	j, err := idem.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{deps: Deps{Journal: j}}
}

func TestDuplicateTokenInFlightRunsOnce(t *testing.T) {
	s := newTokenServer(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var mu sync.Mutex
	runs := 0
	fn := func() (any, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return map[string]string{"ip": "10.0.0.2"}, nil
	}
	recs := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
	var wg sync.WaitGroup
	for _, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.idempotent(rec, httptest.NewRequest(http.MethodPost, "/", nil), "tok", fn)
		}()
		if rec == recs[0] {
			<-started
		}
	}
	// Give the duplicate time to attach to the running flight, then finish the work.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	for _, rec := range recs {
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "10.0.0.2") {
			t.Fatalf("response = %d %s", rec.Code, rec.Body)
		}
	}
}

func TestWaiterTimesOutWithInProgressAndWorkSurvives(t *testing.T) {
	s := newTokenServer(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	runs := 0
	fn := func() (any, error) {
		runs++
		started <- struct{}{}
		<-release
		return map[string]string{"ok": "yes"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.idempotent(rec, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), "tok", fn)
	}()
	<-started
	cancel()
	<-done
	var env apiv1.Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != http.StatusConflict || env.Code != "in_progress" {
		t.Fatalf("timed out wait = %d %s", rec.Code, rec.Body)
	}
	// The detached work finishes after the handler gave up and is journaled for the retry.
	close(release)
	retry := httptest.NewRecorder()
	s.idempotent(retry, httptest.NewRequest(http.MethodPost, "/", nil), "tok", fn)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), "yes") || runs != 1 {
		t.Fatalf("retry = %d %s runs=%d", retry.Code, retry.Body, runs)
	}
}

func TestFailedRunLeavesFlightSoRetryReruns(t *testing.T) {
	s := newTokenServer(t)
	runs := 0
	fn := func() (any, error) {
		runs++
		return nil, errors.New("boom")
	}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.idempotent(rec, httptest.NewRequest(http.MethodPost, "/", nil), "tok", fn)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	if runs != 2 {
		t.Fatalf("runs = %d, want a failed token to rerun", runs)
	}
}

func TestWaitCtxFollowsDeadlineHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(DeadlineHeader, "60000")
	ctx, cancel := waitCtx(req)
	defer cancel()
	dl, ok := ctx.Deadline()
	if got := time.Until(dl); !ok || got > 60*time.Second+deadlineMargin || got < 60*time.Second {
		t.Fatalf("deadline in %s", got)
	}
	bare, cancelBare := waitCtx(httptest.NewRequest(http.MethodPost, "/", nil))
	defer cancelBare()
	if dl, ok := bare.Deadline(); !ok || time.Until(dl) > maxWait {
		t.Fatal("no header must still bound the wait")
	}
}

func TestSnapshotBusyMapsToConflict(t *testing.T) {
	if statusFor(errSnapshotBusy) != http.StatusConflict || codeFor(errSnapshotBusy) != "snapshot_in_progress" {
		t.Fatalf("status=%d code=%s", statusFor(errSnapshotBusy), codeFor(errSnapshotBusy))
	}
}

func TestPanickingWorkReturns500AndReleasesToken(t *testing.T) {
	s := newTokenServer(t)
	rec := httptest.NewRecorder()
	s.idempotent(rec, httptest.NewRequest(http.MethodPost, "/", nil), "tok", func() (any, error) { panic("boom") })
	var env apiv1.Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != http.StatusInternalServerError || env.Code != "host_error" || !strings.Contains(env.Message, "boom") {
		t.Fatalf("panic = %d %s", rec.Code, rec.Body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inflight) != 0 {
		t.Fatal("a panicked run must leave flight")
	}
}

func TestStuckWorkHitsCeilingAndReleasesToken(t *testing.T) {
	s := newTokenServer(t)
	block := make(chan struct{})
	// The detached work journals its result after release; wait for that write
	// so nothing touches the journal's TempDir once the test returns.
	defer func() {
		close(block)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := s.deps.Journal.Lookup("tok"); ok {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("detached work never journaled its result")
	}()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set(DeadlineHeader, "20")
	rec := httptest.NewRecorder()
	s.idempotent(rec, req, "tok", func() (any, error) {
		<-block
		return nil, nil
	})
	var env apiv1.Envelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if rec.Code != http.StatusGatewayTimeout || env.Code != "host_timeout" {
		t.Fatalf("ceiling = %d %s", rec.Code, rec.Body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inflight) != 0 {
		t.Fatal("ceiling must release the in-flight entry")
	}
}

func TestHeaderTokenDedupesWhenBodyTokenEmpty(t *testing.T) {
	s := newTokenServer(t)
	runs := 0
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("idempotency-token", "hdr-1")
		rec := httptest.NewRecorder()
		s.idempotent(rec, req, "", func() (any, error) {
			runs++
			return map[string]string{"dockId": "d1"}, nil
		})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "d1") {
			t.Fatalf("call %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	if runs != 1 {
		t.Fatalf("runs = %d, want the header token to dedupe", runs)
	}
}

func TestCapacityExhaustedMapsToStableCode(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", host.ErrCapacityExhausted)
	if statusFor(err) != http.StatusServiceUnavailable || codeFor(err) != "capacity_exhausted" {
		t.Fatalf("status=%d code=%s", statusFor(err), codeFor(err))
	}
}

func TestEncodeFileReplyCapsEncodedSize(t *testing.T) {
	if _, err := encodeFileReply([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Control bytes JSON-encode to 6 bytes each, so 1.4 MiB overshoots the relay cap.
	bin := bytes.Repeat([]byte{0x01}, 2<<20)
	_, err := encodeFileReply(bin)
	if !errors.Is(err, disk.ErrTooLarge) || statusFor(err) != http.StatusRequestEntityTooLarge || codeFor(err) != "file_too_large" {
		t.Fatalf("err = %v", err)
	}
}

func TestLockDockSerializesAndTimesOut(t *testing.T) {
	old := dockBusyWait
	dockBusyWait = 50 * time.Millisecond
	defer func() { dockBusyWait = old }()
	s := &Server{}
	unlock, err := s.lockDock("d1", "t", dockBusyWait)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.lockDock("d1", "t", dockBusyWait); !errors.Is(err, errDockBusy) {
		t.Fatalf("err = %v", err)
	}
	if statusFor(errDockBusy) != http.StatusConflict || codeFor(errDockBusy) != "dock_busy" {
		t.Fatal("dock_busy mapping")
	}
	other, err := s.lockDock("d2", "t", dockBusyWait)
	if err != nil {
		t.Fatal(err)
	}
	other()
	dockBusyWait = 2 * time.Second
	got := make(chan error, 1)
	go func() {
		u, err := s.lockDock("d1", "t", dockBusyWait)
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(20 * time.Millisecond)
	unlock()
	if err := <-got; err != nil {
		t.Fatalf("waiter err = %v", err)
	}
}

func TestLockWaitFollowsCommandDeadline(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/destroy", nil)
	if lockWait(req) != dockBusyWait {
		t.Fatal("no header keeps the default")
	}
	req.Header.Set(DeadlineHeader, "10000")
	if got := lockWait(req); got != 8*time.Second {
		t.Fatalf("wait = %s", got)
	}
	req.Header.Set(DeadlineHeader, "600000")
	if lockWait(req) != dockBusyWait {
		t.Fatal("long deadlines stay capped")
	}
	req.Header.Set(DeadlineHeader, "1000")
	if lockWait(req) != 0 {
		t.Fatal("tiny deadlines do not wait")
	}
}

func TestSnapshotCreateWaitsOnDockLock(t *testing.T) {
	s := &Server{}
	unlock, err := s.lockDock("d1", "destroy-1", dockBusyWait)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	body := `{"dockId":"d1","idempotencyToken":"snap-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/snapshots/create", strings.NewReader(body))
	req.Header.Set(DeadlineHeader, "2100")
	rec := httptest.NewRecorder()
	s.handleSnapshotCreate(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "dock_busy") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if !s.tryAcquire("d1") {
		t.Fatal("capture slot must be released after a busy answer")
	}
}

func TestLogHeldDocksNamesStuckDock(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	s := &Server{}
	unlock, _ := s.lockDock("d1", "tok", dockBusyWait)
	defer unlock()
	s.logHeldDocks("other")
	if buf.Len() != 0 {
		t.Fatal("unrelated token must not log")
	}
	s.logHeldDocks("tok")
	if !strings.Contains(buf.String(), `dock "d1" stays locked`) {
		t.Fatalf("log = %q", buf.String())
	}
}
