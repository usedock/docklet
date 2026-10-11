package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
)

type recorded struct {
	method string
	url    string
	header http.Header
	body   []byte
}

// fakeTransport records calls and answers them from a handler; tests assert on
// the wire shape without binding a socket.
type fakeTransport struct {
	mu      sync.Mutex
	calls   []recorded
	handler func(recorded) (*http.Response, error)
}

func (f *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = raw
	}
	call := recorded{method: req.Method, url: req.URL.String(), header: req.Header.Clone(), body: body}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return nil, fmt.Errorf("unexpected request %s %s", call.method, call.url)
	}
	return handler(call)
}

func (f *fakeTransport) callsMatching(fragment string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, call := range f.calls {
		if strings.Contains(call.url, fragment) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"content-type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

// inventory is a static orchestrator for tests.
type inventory struct{ running []apiv1.RunningVM }

func (i inventory) Running() []apiv1.RunningVM { return i.running }

// samplerInventory answers stats for d1 only; everything else fails.
type samplerInventory struct{ inventory }

func (s samplerInventory) Stats(_ context.Context, dockID string) (apiv1.StatsSample, error) {
	if dockID != "d1" {
		return apiv1.StatsSample{}, fmt.Errorf("no stats for %s", dockID)
	}
	return apiv1.StatsSample{DockID: dockID, CPUPercent: 1.5, MemBytes: 1 << 20, MemLimit: 2 << 30}, nil
}

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testAgent(t *testing.T, transport Doer, host Orchestrator) Config {
	t.Helper()
	root := fixture(t, map[string]string{
		"proc/cpuinfo": "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel\t\t: 1\nstepping\t: 2\n\n" +
			"processor\t: 1\nvendor_id\t: AuthenticAMD\nmodel\t\t: 1\nstepping\t: 2\n",
		"proc/meminfo":              "MemTotal:       32768 kB\n",
		"proc/sys/kernel/osrelease": "7.0.0-test\n",
	})
	return Config{
		FleetURL:     "https://api.usedock.test",
		Token:        "fleet-token",
		HostID:       "host-1",
		Runtime:      "gvisor",
		LocalAPIBase: "http://127.0.0.1:8081",
		Client:       transport,
		Host:         host,
		Root:         root,
		Logf:         t.Logf,
	}
}

func TestRegisterCarriesConfiguredRuntimeAndCapacity(t *testing.T) {
	for _, runtime := range []string{"microvm", "gvisor"} {
		t.Run(runtime, func(t *testing.T) {
			transport := &fakeTransport{}
			transport.handler = func(recorded) (*http.Response, error) {
				return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1","heartbeatSeconds":30,"pollWaitSeconds":25}}`), nil
			}
			cfg := testAgent(t, transport, inventory{})
			cfg.Runtime = runtime

			reg, err := cfg.register(context.Background())
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			calls := transport.callsMatching("/v1/fleet/register")
			if len(calls) != 1 {
				t.Fatalf("register calls = %d, want 1", len(calls))
			}
			call := calls[0]
			if call.method != http.MethodPost {
				t.Fatalf("register method = %s, want POST", call.method)
			}
			if call.url != "https://api.usedock.test/v1/fleet/register" {
				t.Fatalf("register url = %s", call.url)
			}
			if got := call.header.Get("authorization"); got != "Bearer fleet-token" {
				t.Fatalf("register authorization = %q", got)
			}
			var body registerRequest
			if err := json.Unmarshal(call.body, &body); err != nil {
				t.Fatalf("register body: %v", err)
			}
			if body.HostID != "host-1" || body.Runtime != runtime {
				t.Fatalf("register = %+v, want hostId host-1 runtime %s", body, runtime)
			}
			// Runtime validation is the control plane's job: this agent only
			// ever reports the configured runtime.
			if bytes.Contains(call.body, []byte("pvm")) {
				t.Fatalf("register body invents a runtime: %s", call.body)
			}
			if body.CPUFamily != "AuthenticAMD:1:2" {
				t.Fatalf("cpuFamily = %q", body.CPUFamily)
			}
			if body.Kernel != "7.0.0-test" || body.KernelRelease != "7.0.0-test" {
				t.Fatalf("kernel = %q/%q", body.Kernel, body.KernelRelease)
			}
			if body.VcpuTotal != 2 {
				t.Fatalf("vcpuTotal = %d, want 2", body.VcpuTotal)
			}
			if body.MemTotalBytes != 32768*1024 {
				t.Fatalf("memTotalBytes = %d", body.MemTotalBytes)
			}
			if body.DiskTotalBytes <= 0 {
				t.Fatalf("diskTotalBytes = %d", body.DiskTotalBytes)
			}
			if body.MaxDocks < 1 {
				t.Fatalf("maxDocks = %d", body.MaxDocks)
			}
			if reg.HeartbeatSeconds != 30 || reg.PollWaitSeconds != 25 {
				t.Fatalf("register response = %+v", reg)
			}
		})
	}
}

func TestRegisterUsesConfiguredFamily(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Family = "GenuineIntel:106:6"

	if _, err := cfg.register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	calls := transport.callsMatching("/v1/fleet/register")
	if len(calls) != 1 {
		t.Fatalf("register calls = %d, want 1", len(calls))
	}
	var body registerRequest
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatalf("register body: %v", err)
	}
	if body.CPUFamily != "GenuineIntel:106:6" {
		t.Fatalf("cpuFamily = %q, want the orchestrator's family", body.CPUFamily)
	}
}

func TestExecuteRelaysPostOpWithIdempotencyHeader(t *testing.T) {
	const relayed = `{"ip":"192.168.127.2","hostId":"host-1"}`
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		switch {
		case strings.HasPrefix(call.url, "http://127.0.0.1:8081/v1/provision"):
			return jsonResponse(http.StatusOK, `{"ok":true,"data":`+relayed+`}`), nil
		case strings.Contains(call.url, "/v1/fleet/commands/c1/result"):
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		return nil, fmt.Errorf("unexpected %s %s", call.method, call.url)
	}
	cfg := testAgent(t, transport, inventory{})
	payload := `{"dockId":"d1","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10},"idempotencyToken":"tok-1"}`

	cfg.execute(context.Background(), command{
		ID: "c1", Op: "provision", IdempotencyToken: "tok-1", Payload: json.RawMessage(payload),
	})

	relayedCalls := transport.callsMatching("/v1/provision")
	if len(relayedCalls) != 1 {
		t.Fatalf("relay calls = %d, want 1", len(relayedCalls))
	}
	relay := relayedCalls[0]
	if relay.method != http.MethodPost {
		t.Fatalf("relay method = %s, want POST", relay.method)
	}
	if relay.url != "http://127.0.0.1:8081/v1/provision" {
		t.Fatalf("relay url = %s", relay.url)
	}
	if got := relay.header.Get("idempotency-token"); got != "tok-1" {
		t.Fatalf("relay idempotency-token = %q, want tok-1", got)
	}
	if string(relay.body) != payload {
		t.Fatalf("relay body = %s, want verbatim %s", relay.body, payload)
	}

	results := transport.callsMatching("/result")
	if len(results) != 1 {
		t.Fatalf("result posts = %d, want 1", len(results))
	}
	if results[0].url != "https://api.usedock.test/v1/fleet/commands/c1/result" {
		t.Fatalf("result url = %s", results[0].url)
	}
	var result resultRequest
	if err := json.Unmarshal(results[0].body, &result); err != nil {
		t.Fatalf("result body: %v", err)
	}
	if !result.Ok || result.HostID != "host-1" {
		t.Fatalf("result = %+v, want ok for host-1", result)
	}
	if got, want := compact(t, result.Data), compact(t, []byte(relayed)); got != want {
		t.Fatalf("relayed data = %s, want %s", got, want)
	}
}

func TestRelayGetOpsBuildLocalURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		op        string
		payload   string
		wantPath  string
		wantQuery map[string]string
	}{
		{"stats", "stats", `{"dock":"dock-7"}`, "/v1/stats", map[string]string{"dock": "dock-7"}},
		{"tree", "generation.tree", `{"snapshotId":"g-9"}`, "/v1/generations/g-9/tree", nil},
		{"file", "generation.file", `{"snapshotId":"g-9","path":"/etc/hosts"}`, "/v1/generations/g-9/files", map[string]string{"path": "/etc/hosts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{}
			transport.handler = func(call recorded) (*http.Response, error) {
				if strings.Contains(call.url, "/result") {
					return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
				}
				return jsonResponse(http.StatusOK, `{"ok":true,"data":{"dockId":"dock-7"}}`), nil
			}
			cfg := testAgent(t, transport, inventory{})

			cfg.execute(context.Background(), command{ID: "c1", Op: tc.op, Payload: json.RawMessage(tc.payload)})

			relays := transport.callsMatching("127.0.0.1:8081")
			if len(relays) != 1 {
				t.Fatalf("relay calls = %d, want 1", len(relays))
			}
			if relays[0].method != http.MethodGet {
				t.Fatalf("relay method = %s, want GET", relays[0].method)
			}
			if got := relays[0].header.Get("idempotency-token"); got != "" {
				t.Fatalf("GET relay carries idempotency-token %q", got)
			}
			parsed, err := url.Parse(relays[0].url)
			if err != nil {
				t.Fatalf("relay url %s: %v", relays[0].url, err)
			}
			if parsed.Scheme != "http" || parsed.Host != "127.0.0.1:8081" || parsed.Path != tc.wantPath {
				t.Fatalf("relay url = %s, want path %s", relays[0].url, tc.wantPath)
			}
			for key, want := range tc.wantQuery {
				if got := parsed.Query().Get(key); got != want {
					t.Fatalf("relay url %s query %s = %q, want %q", relays[0].url, key, got, want)
				}
			}
		})
	}
}

func TestExecuteRelayFailurePostsDockletCode(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/result") {
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		return jsonResponse(http.StatusServiceUnavailable, `{"ok":false,"code":"host_not_ready","message":"host not ready: no-kvm"}`), nil
	}
	cfg := testAgent(t, transport, inventory{})

	cfg.execute(context.Background(), command{ID: "c2", Op: "destroy", IdempotencyToken: "tok-2", Payload: json.RawMessage(`{"dockId":"d1"}`)})

	results := transport.callsMatching("/result")
	if len(results) != 1 {
		t.Fatalf("result posts = %d, want 1", len(results))
	}
	var result resultRequest
	if err := json.Unmarshal(results[0].body, &result); err != nil {
		t.Fatalf("result body: %v", err)
	}
	if result.Ok || result.Code != "host_not_ready" || result.Message != "host not ready: no-kvm" {
		t.Fatalf("result = %+v, want relayed code and message", result)
	}
	if len(result.Data) != 0 {
		t.Fatalf("failure result carries data: %s", result.Data)
	}
}

func TestExecuteRelayTransportErrorPostsHostUnreachable(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/result") {
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		return nil, errors.New("dial tcp 127.0.0.1:8081: connect: connection refused")
	}
	cfg := testAgent(t, transport, inventory{})

	cfg.execute(context.Background(), command{ID: "c3", Op: "status"})

	results := transport.callsMatching("/result")
	if len(results) != 1 {
		t.Fatalf("result posts = %d, want 1", len(results))
	}
	var result resultRequest
	if err := json.Unmarshal(results[0].body, &result); err != nil {
		t.Fatalf("result body: %v", err)
	}
	if result.Ok || result.Code != "host_unreachable" {
		t.Fatalf("result = %+v, want host_unreachable", result)
	}
	if !strings.Contains(result.Message, "connection refused") {
		t.Fatalf("result message = %q", result.Message)
	}
}

func TestHeartbeatCarriesRunningInventory(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, samplerInventory{inventory{running: []apiv1.RunningVM{
		{DockID: "d1", State: "running", Kind: "dock"},
		{DockID: "f1", State: "running", Kind: "fast"},
	}}})

	cfg.Runtime = "gvisor"
	cfg.heartbeat(context.Background())

	calls := transport.callsMatching("/v1/fleet/heartbeat")
	if len(calls) != 1 {
		t.Fatalf("heartbeat calls = %d, want 1", len(calls))
	}
	var body heartbeatRequest
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatalf("heartbeat body: %v", err)
	}
	if body.Runtime != "gvisor" {
		t.Fatalf("heartbeat runtime = %q, want gvisor", body.Runtime)
	}
	if body.HostID != "host-1" || body.UsedDocks != 2 {
		t.Fatalf("heartbeat = %+v, want 2 used docks", body)
	}
	if len(body.Running) != 2 || body.Running[0].DockID != "d1" || body.Running[1].Kind != "fast" {
		t.Fatalf("heartbeat running = %+v", body.Running)
	}
	// The unsampleable dock is omitted instead of failing the heartbeat.
	if len(body.Samples) != 1 || body.Samples[0].DockID != "d1" || body.Samples[0].Source != "host" {
		t.Fatalf("heartbeat samples = %+v", body.Samples)
	}
}

func TestHeartbeatReportsHeldBases(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Bases = func() []string { return []string{fp} }
	cfg.heartbeat(context.Background())
	// No bases held is reported as an explicit empty list, never omitted.
	cfg.Bases = func() []string { return []string{} }
	cfg.heartbeat(context.Background())
	// A gvisor host reports nothing at all.
	cfg.Bases = nil
	cfg.heartbeat(context.Background())

	calls := transport.callsMatching("/v1/fleet/heartbeat")
	if len(calls) != 3 {
		t.Fatalf("heartbeat calls = %d, want 3", len(calls))
	}
	if !strings.Contains(string(calls[0].body), `"baseFingerprints":["`+fp+`"]`) {
		t.Fatalf("first heartbeat = %s, want the held base", calls[0].body)
	}
	if !strings.Contains(string(calls[1].body), `"baseFingerprints":[]`) {
		t.Fatalf("second heartbeat = %s, want an empty base list", calls[1].body)
	}
	if strings.Contains(string(calls[2].body), "baseFingerprints") {
		t.Fatalf("third heartbeat = %s, want no base list", calls[2].body)
	}
}

func TestHeartbeatReportsServedImages(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Images = func() []string { return []string{"node-22", "ubuntu-24.04"} }
	cfg.heartbeat(context.Background())
	cfg.Images = func() []string { return []string{} }
	cfg.heartbeat(context.Background())
	cfg.Images = nil
	cfg.heartbeat(context.Background())

	calls := transport.callsMatching("/v1/fleet/heartbeat")
	if len(calls) != 3 {
		t.Fatalf("heartbeat calls = %d, want 3", len(calls))
	}
	if !strings.Contains(string(calls[0].body), `"images":["node-22","ubuntu-24.04"]`) {
		t.Fatalf("first heartbeat = %s, want the served images", calls[0].body)
	}
	if !strings.Contains(string(calls[1].body), `"images":[]`) {
		t.Fatalf("second heartbeat = %s, want an explicit empty list", calls[1].body)
	}
	if strings.Contains(string(calls[2].body), `"images"`) {
		t.Fatalf("third heartbeat = %s, want no images field", calls[2].body)
	}
}

func TestHeartbeatDropsImagesForOlderBackends(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(string(call.body), `"images"`) {
			return jsonResponse(http.StatusBadRequest, `{"ok":false,"code":"validation_error","message":"property images should not exist"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Images = func() []string { return []string{"node-22"} }
	cfg.heartbeat(context.Background())

	calls := transport.callsMatching("/v1/fleet/heartbeat")
	if len(calls) != 2 || strings.Contains(string(calls[1].body), `"images"`) {
		t.Fatalf("heartbeat calls = %d, want a retry without images", len(calls))
	}
}

func TestHeartbeatDropsBasesForOlderBackends(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(string(call.body), "baseFingerprints") {
			return jsonResponse(http.StatusBadRequest, `{"ok":false,"code":"validation_error","message":"property baseFingerprints should not exist"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.Bases = func() []string { return []string{strings.Repeat("cd", 32)} }
	cfg.heartbeat(context.Background())

	calls := transport.callsMatching("/v1/fleet/heartbeat")
	if len(calls) != 2 || strings.Contains(string(calls[1].body), "baseFingerprints") {
		t.Fatalf("heartbeat calls = %d, want a retry without the base list", len(calls))
	}
}

func TestRunRegistersRelaysAndStopsOnCancel(t *testing.T) {
	var (
		mu      sync.Mutex
		polls   int
		relayed bool
	)
	heartbeat := make(chan struct{}, 1)
	result := make(chan struct{}, 1)
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		switch {
		case strings.HasSuffix(call.url, "/v1/fleet/register"):
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1","heartbeatSeconds":1,"pollWaitSeconds":1}}`), nil
		case strings.HasSuffix(call.url, "/v1/fleet/heartbeat"):
			select {
			case heartbeat <- struct{}{}:
			default:
			}
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
		case strings.Contains(call.url, "/v1/fleet/commands?"):
			parsed, err := url.Parse(call.url)
			if err != nil {
				return nil, err
			}
			if parsed.Query().Get("hostId") != "host-1" || parsed.Query().Get("wait") != "1" {
				return nil, fmt.Errorf("poll query = %s", parsed.RawQuery)
			}
			mu.Lock()
			polls++
			first := polls == 1
			mu.Unlock()
			if !first {
				return jsonResponse(http.StatusOK, `{"ok":true,"data":{"command":null}}`), nil
			}
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"command":{"id":"c1","op":"status","payload":{}}}}`), nil
		case strings.HasPrefix(call.url, "http://127.0.0.1:8081/v1/status"):
			mu.Lock()
			relayed = true
			mu.Unlock()
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1","running":[]}}`), nil
		case strings.Contains(call.url, "/v1/fleet/commands/c1/result"):
			result <- struct{}{}
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		return nil, fmt.Errorf("unexpected %s %s", call.method, call.url)
	}
	cfg := testAgent(t, transport, inventory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cfg.Run(ctx) }()

	for name, ch := range map[string]chan struct{}{"heartbeat": heartbeat, "result": result} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", name)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if !relayed {
		t.Fatal("command was never relayed to the local API")
	}
}

func TestAttemptStopsOnPermanentRejection(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"ok":false,"code":"validation_error","message":"bad backend"}`), nil
	}
	cfg := testAgent(t, transport, inventory{})

	err := cfg.attempt(context.Background(), "register", func(ctx context.Context) error {
		return cfg.fleet(ctx, http.MethodPost, "/v1/fleet/register", registerRequest{}, nil, 0)
	})
	if err == nil {
		t.Fatal("permanent rejection must surface as an error")
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("attempts = %d, want 1 for a permanent rejection", got)
	}
}

func TestAttemptRetriesTransientFailure(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if transport.count() == 1 {
			return jsonResponse(http.StatusServiceUnavailable, `{"ok":false,"code":"unavailable","message":"restarting"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{"hostId":"host-1"}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})

	if err := cfg.attempt(context.Background(), "heartbeat", func(ctx context.Context) error {
		return cfg.fleet(ctx, http.MethodPost, "/v1/fleet/heartbeat", heartbeatRequest{HostID: cfg.HostID}, nil, 0)
	}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestAttemptKeepsRetryingUnauthorized(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"ok":false,"code":"unauthorized"}`), nil
	}
	cfg := testAgent(t, transport, inventory{})

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	err := cfg.attempt(ctx, "heartbeat", func(ctx context.Context) error {
		return cfg.fleet(ctx, http.MethodPost, "/v1/fleet/heartbeat", heartbeatRequest{HostID: cfg.HostID}, nil, 0)
	})
	if err == nil {
		t.Fatal("ctx cancellation must surface as an error")
	}
	if got := transport.count(); got < 2 {
		t.Fatalf("attempts = %d, want a 401 retried with backoff", got)
	}
}

func TestNextBackoffIsBounded(t *testing.T) {
	if got := nextBackoff(time.Second, false); got != 2*time.Second {
		t.Fatalf("nextBackoff(1s) = %s, want 2s", got)
	}
	if got := nextBackoff(time.Minute, false); got != backoffMax {
		t.Fatalf("nextBackoff(1m) = %s, want %s", got, backoffMax)
	}
	if got := nextBackoff(time.Second, true); got != backoffMax {
		t.Fatalf("nextBackoff(permanent) = %s, want %s", got, backoffMax)
	}
}

func TestRunRejectsMissingConfig(t *testing.T) {
	cfg := testAgent(t, &fakeTransport{}, inventory{})
	cfg.Token = ""
	if err := cfg.Run(context.Background()); err == nil {
		t.Fatal("Run without a token must fail")
	}
}

func TestDispatchRunsExecBesideLifecycleOps(t *testing.T) {
	release := make(chan struct{})
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		switch {
		case strings.HasPrefix(call.url, "http://127.0.0.1:8081/v1/fast-runs/exec"):
			<-release
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"exitCode":0}}`), nil
		case strings.HasPrefix(call.url, "http://127.0.0.1:8081/v1/pause"):
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"dockId":"d2"}}`), nil
		case strings.Contains(call.url, "/result"):
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		return nil, fmt.Errorf("unexpected %s %s", call.method, call.url)
	}
	cfg := testAgent(t, transport, inventory{})
	slots := make(chan struct{}, 1)
	var running sync.WaitGroup
	lc := newLifecycle()
	ctx := context.Background()
	cfg.dispatch(ctx, command{ID: "exec-1", Op: "fast-run.exec", IdempotencyToken: "e1", Payload: json.RawMessage(`{"runtimeId":"d1"}`)}, slots, lc, &running)
	// The lifecycle op completes while the exec is still running.
	cfg.dispatch(ctx, command{ID: "pause-1", Op: "pause", Payload: json.RawMessage(`{"dockId":"d2"}`)}, slots, lc, &running)
	eventually(t, func() bool { return len(transport.callsMatching("/commands/pause-1/result")) == 1 }, "lifecycle op must not wait behind a running exec")
	// A full host refuses further execs instead of blocking the loop.
	cfg.dispatch(ctx, command{ID: "exec-2", Op: "fast-run.exec", IdempotencyToken: "e2", Payload: json.RawMessage(`{"runtimeId":"d1"}`)}, slots, lc, &running)
	busy := transport.callsMatching("/commands/exec-2/result")
	if len(busy) != 1 {
		t.Fatalf("busy result posts = %d", len(busy))
	}
	var result resultRequest
	if err := json.Unmarshal(busy[0].body, &result); err != nil || result.Ok || result.Code != "host_busy" {
		t.Fatalf("busy result = %+v %v", result, err)
	}
	close(release)
	running.Wait()
	if len(transport.callsMatching("/commands/exec-1/result")) != 1 {
		t.Fatal("exec result must post once it finishes")
	}
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// gatedTransport answers relays after the test releases them, recording start order.
type gatedTransport struct {
	fakeTransport
	started chan string
	gates   sync.Map
}

func (g *gatedTransport) gate(name string) chan struct{} {
	ch, _ := g.gates.LoadOrStore(name, make(chan struct{}))
	return ch.(chan struct{})
}

func newGatedTransport() *gatedTransport {
	g := &gatedTransport{started: make(chan string, 64)}
	g.handler = func(call recorded) (*http.Response, error) {
		if strings.Contains(call.url, "/result") {
			return jsonResponse(http.StatusOK, `{"ok":true,"data":{"accepted":true}}`), nil
		}
		var body struct {
			DockID string `json:"dockId"`
		}
		_ = json.Unmarshal(call.body, &body)
		name := strings.TrimPrefix(call.url, "http://127.0.0.1:8081") + "#" + body.DockID
		g.started <- name
		<-g.gate(name)
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{}}`), nil
	}
	return g
}

func TestLifecycleOpDoesNotDelayInteractiveOps(t *testing.T) {
	g := newGatedTransport()
	cfg := testAgent(t, &g.fakeTransport, inventory{})
	slots := make(chan struct{}, 4)
	var running sync.WaitGroup
	lc := newLifecycle()
	ctx := context.Background()
	cfg.dispatch(ctx, command{ID: "snap", Op: "snapshot.create", Payload: json.RawMessage(`{"dockId":"d1"}`)}, slots, lc, &running)
	if got := <-g.started; got != "/v1/snapshots/create#d1" {
		t.Fatalf("started %s", got)
	}
	// The snapshot is still blocked while an exec and a stats read for other docks complete.
	cfg.dispatch(ctx, command{ID: "exec", Op: "fast-run.exec", Payload: json.RawMessage(`{"runtimeId":"r1"}`)}, slots, lc, &running)
	<-g.started
	close(g.gate("/v1/fast-runs/exec#"))
	eventually(t, func() bool { return len(g.callsMatching("/commands/exec/result")) == 1 }, "exec must finish while a snapshot runs")
	if len(g.callsMatching("/commands/snap/result")) != 0 {
		t.Fatal("snapshot must still be running")
	}
	close(g.gate("/v1/snapshots/create#d1"))
	running.Wait()
}

func TestLifecycleSerializesPerDockOnly(t *testing.T) {
	g := newGatedTransport()
	cfg := testAgent(t, &g.fakeTransport, inventory{})
	slots := make(chan struct{}, 4)
	var running sync.WaitGroup
	lc := newLifecycle()
	ctx := context.Background()
	cfg.dispatch(ctx, command{ID: "a", Op: "provision", Payload: json.RawMessage(`{"dockId":"d1"}`)}, slots, lc, &running)
	cfg.dispatch(ctx, command{ID: "b", Op: "snapshot.create", Payload: json.RawMessage(`{"dockId":"d1"}`)}, slots, lc, &running)
	cfg.dispatch(ctx, command{ID: "c", Op: "provision", Payload: json.RawMessage(`{"dockId":"d2"}`)}, slots, lc, &running)
	first := map[string]bool{<-g.started: true, <-g.started: true}
	if !first["/v1/provision#d1"] || !first["/v1/provision#d2"] {
		t.Fatalf("expected d1 and d2 provisions to start together, got %v", first)
	}
	select {
	case name := <-g.started:
		t.Fatalf("%s overlapped a running lifecycle op on the same dock", name)
	case <-time.After(100 * time.Millisecond):
	}
	close(g.gate("/v1/provision#d1"))
	if got := <-g.started; got != "/v1/snapshots/create#d1" {
		t.Fatalf("queued op started %s", got)
	}
	close(g.gate("/v1/provision#d2"))
	close(g.gate("/v1/snapshots/create#d1"))
	running.Wait()
}

func TestLifecycleRefusesWhenQueueFull(t *testing.T) {
	g := newGatedTransport()
	cfg := testAgent(t, &g.fakeTransport, inventory{})
	slots := make(chan struct{}, 1)
	var running sync.WaitGroup
	lc := newLifecycle()
	ctx := context.Background()
	for i := 0; i < maxPendingLifecycle; i++ {
		cfg.dispatch(ctx, command{ID: fmt.Sprintf("p%d", i), Op: "pause", Payload: json.RawMessage(`{"dockId":"d1"}`)}, slots, lc, &running)
	}
	cfg.dispatch(ctx, command{ID: "over", Op: "pause", Payload: json.RawMessage(`{"dockId":"d1"}`)}, slots, lc, &running)
	busy := g.callsMatching("/commands/over/result")
	var result resultRequest
	if len(busy) != 1 || json.Unmarshal(busy[0].body, &result) != nil || result.Code != "host_busy" {
		t.Fatalf("overflow result = %+v", busy)
	}
	close(g.gate("/v1/pause#d1"))
	running.Wait()
}

func TestRelaySendsCommandDeadlineHeader(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"ok":true,"data":{}}`), nil
	}
	cfg := testAgent(t, transport, inventory{})
	cfg.relay(context.Background(), command{ID: "c", Op: "provision", DeadlineMs: 90000, Payload: json.RawMessage(`{"dockId":"d1"}`)})
	calls := transport.callsMatching("/v1/provision")
	if len(calls) != 1 || calls[0].header.Get("X-Deadline-Ms") != "90000" {
		t.Fatalf("deadline header = %+v", calls)
	}
}

func TestRedeliveredRunningCommandRunsOnce(t *testing.T) {
	g := newGatedTransport()
	cfg := testAgent(t, &g.fakeTransport, inventory{})
	slots := make(chan struct{}, 4)
	var running sync.WaitGroup
	lc := newLifecycle()
	ctx := context.Background()
	cmd := command{ID: "snap", Op: "snapshot.load", Payload: json.RawMessage(`{"dockId":"d1"}`)}
	cfg.dispatch(ctx, cmd, slots, lc, &running)
	<-g.started
	// The control plane redelivers the still-running command: it must be dropped, not queued.
	cfg.dispatch(ctx, cmd, slots, lc, &running)
	cfg.dispatch(ctx, cmd, slots, lc, &running)
	close(g.gate("/v1/snapshots/load#d1"))
	running.Wait()
	if got := len(g.callsMatching("/v1/snapshots/load")); got != 1 {
		t.Fatalf("relays = %d, want 1", got)
	}
	if got := len(g.callsMatching("/commands/snap/result")); got != 1 {
		t.Fatalf("results = %d, want 1", got)
	}
	// Once finished, the ID is free again.
	cfg.dispatch(ctx, cmd, slots, lc, &running)
	running.Wait()
	if got := len(g.callsMatching("/commands/snap/result")); got != 2 {
		t.Fatalf("results after re-run = %d", got)
	}
}
