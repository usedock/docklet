package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/host"
	"github.com/usedock/docklet/internal/ids"
)

func post(mux *http.ServeMux, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1", Token: testToken})
	for _, path := range []string{"/v1/provision", "/v1/pause", "/v1/snapshots/create", "/v1/snapshots/load", "/v1/destroy", "/v1/fast-runs/create", "/v1/fast-runs/exec", "/v1/fast-runs/read", "/v1/fast-runs/terminate"} {
		for _, token := range []string{"", "wrong"} {
			if rec := post(mux, path, "{}", token); rec.Code != http.StatusUnauthorized {
				t.Fatalf("POST %s token=%q = %d, want 401", path, token, rec.Code)
			}
		}
	}
	for _, path := range []string{"/v1/status", "/v1/stats?dock=d1", "/v1/heartbeat", "/v1/generations/g1/tree", "/v1/generations/g1/files?path=x"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, want 401", path, rec.Code)
		}
	}
}

func TestUnsetTokenRefusesEverything(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1"})
	if rec := post(mux, "/v1/destroy", "{}", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("destroy without a configured token = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty bearer against an empty token = %d, want 401", rec.Code)
	}
}

func TestHealthAndReadinessStayOpen(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1", Token: testToken})
	for _, path := range []string{"/v1/healthz", "/v1/readiness"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 without a token (bootstrap probe)", path, rec.Code)
		}
	}
}

func TestTraversalIDsAreRejectedAtTheBoundary(t *testing.T) {
	mux := NewMux(Deps{HostID: "h1", Token: testToken, Host: &host.Host{}})
	cases := []struct{ path, body string }{
		{"/v1/provision", `{"dockId":"../../etc","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10}}`},
		{"/v1/provision", `{"dockId":"d1","resources":{"cpus":1,"memoryMiB":2048,"diskGiB":10},"snapshotId":"../x"}`},
		{"/v1/pause", `{"dockId":"a/b"}`},
		{"/v1/destroy", `{"dockId":"../.."}`},
		{"/v1/destroy", `{"dockId":""}`},
		{"/v1/snapshots/create", `{"dockId":"..","kind":"full"}`},
		{"/v1/snapshots/create", `{"dockId":"d1","parentId":"../p","kind":"full"}`},
		{"/v1/snapshots/load", `{"dockId":"d1","snapshotId":"x/../y"}`},
		{"/v1/fast-runs/create", `{"runtimeId":"../r","guestToken":"g","idempotencyToken":"i"}`},
		{"/v1/fast-runs/exec", `{"runtimeId":"a b","script":"true","idempotencyToken":"i"}`},
		{"/v1/fast-runs/read", `{"runtimeId":"a/b","path":"/x","idempotencyToken":"i"}`},
		{"/v1/fast-runs/terminate", `{"runtimeId":"..","idempotencyToken":"i"}`},
	}
	for _, c := range cases {
		rec := post(mux, c.path, c.body, testToken)
		var env apiv1.Envelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if rec.Code != http.StatusBadRequest || env.Code != "validation_error" {
			t.Fatalf("POST %s %s = %d %+v, want 400 validation_error", c.path, c.body, rec.Code, env)
		}
	}
	for _, path := range []string{"/v1/stats?dock=..%2Fx", "/v1/stats", "/v1/generations/a%20b/tree", "/v1/generations/..%2Fx/files?path=x"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400", path, rec.Code)
		}
	}
}

func TestInvalidIDErrorMapsToBadRequest(t *testing.T) {
	err := fmt.Errorf("x: %w", ids.ErrInvalid)
	if statusFor(err) != http.StatusBadRequest || codeFor(err) != "validation_error" {
		t.Fatalf("status=%d code=%s", statusFor(err), codeFor(err))
	}
}

func TestNewTokenUsesConfiguredValueAndStillWritesFile(t *testing.T) {
	dir := t.TempDir()
	token, err := NewToken(dir, " shared-secret ")
	if err != nil || token != "shared-secret" {
		t.Fatalf("token = %q %v, want the configured value", token, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, TokenFile))
	if strings.TrimSpace(string(raw)) != "shared-secret" {
		t.Fatalf("file = %q, the agent reads the configured token from here", raw)
	}
}

func TestNewTokenWritesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	token, err := NewToken(dir, "")
	if err != nil || len(token) < 32 {
		t.Fatalf("token = %q %v", token, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil || strings.TrimSpace(string(raw)) != token {
		t.Fatalf("file = %q %v, want the token", raw, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, TokenFile))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
		}
	}
	other, _ := NewToken(t.TempDir(), "")
	if other == token {
		t.Fatal("tokens must be random per start")
	}
}
