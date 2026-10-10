package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	dockA = "11111111-1111-4111-8111-111111111111"
	dockB = "22222222-2222-4222-8222-222222222222"
	wsID  = "33333333-3333-4333-8333-333333333333"
	orgID = "44444444-4444-4444-8444-444444444444"
)

type recorded struct {
	Method, URI, Auth, Confirm string
	Body                       map[string]any
}

// fakeAPI is an httptest fake of the control plane /v1 API.
type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// routes maps "METHOD /path" to a handler returning status and data; a non-nil errCode makes an error envelope.
	routes map[string]func(body map[string]any) (int, any)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{routes: map[string]func(map[string]any) (int, any){}}
	dockJSON := func(id, name, state string) map[string]any {
		return map[string]any{"id": id, "name": name, "state": state, "image": "ubuntu-24.04", "runtime": "microvm",
			"environment": "base", "cpus": 2, "memoryMiB": 4096, "diskGiB": 20}
	}
	f.routes["GET /v1/organizations"] = func(map[string]any) (int, any) { return 200, []any{map[string]any{"id": orgID}} }
	f.routes["GET /v1/organizations/"+orgID+"/workspaces"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": wsID}}
	}
	f.routes["GET /v1/docks"] = func(map[string]any) (int, any) {
		return 200, []any{dockJSON(dockA, "web", "ready"), dockJSON(dockB, "db", "archived")}
	}
	f.routes["POST /v1/docks"] = func(b map[string]any) (int, any) {
		d := dockJSON(dockA, "web", "ready")
		if n, ok := b["name"].(string); ok {
			d["name"] = n
		}
		return 201, d
	}
	f.routes["POST /v1/docks/"+dockA+"/stop"] = func(map[string]any) (int, any) {
		return 202, map[string]any{"id": dockA, "state": "archived"}
	}
	f.routes["POST /v1/docks/"+dockA+"/resume"] = func(map[string]any) (int, any) {
		return 202, map[string]any{"id": dockA, "state": "ready"}
	}
	f.routes["POST /v1/docks/"+dockA+"/branch"] = func(map[string]any) (int, any) {
		return 201, dockJSON(dockB, "web-2", "ready")
	}
	f.routes["DELETE /v1/docks/"+dockA] = func(map[string]any) (int, any) {
		return 202, map[string]any{"operation": map[string]any{"id": "op1", "state": "completed"}}
	}
	f.routes["POST /v1/docks/"+dockA+"/exec"] = func(map[string]any) (int, any) {
		return 200, map[string]any{"exitCode": 0, "stdout": "hello\n", "stderr": "", "durationMs": 5, "truncated": false}
	}
	f.routes["GET /v1/config"] = func(map[string]any) (int, any) {
		return 200, map[string]any{"ssh": map[string]any{"host": "dock.example.com", "port": 2222}}
	}
	f.routes["POST /v1/docks/"+dockA+"/ports"] = func(b map[string]any) (int, any) {
		return 201, map[string]any{"port": b["port"], "url": "https://3000-abc.ports.example.com"}
	}
	f.routes["DELETE /v1/docks/"+dockA+"/ports/3000"] = func(map[string]any) (int, any) {
		return 200, map[string]any{"removed": true}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("x-confirm-delete"), body})
		h := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if h == nil {
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "code": "not_found", "message": "no route"})
			return
		}
		status, data := h(body)
		if e, ok := data.(apiErr); ok {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "code": e.code, "message": e.msg})
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type apiErr struct{ code, msg string }

func (f *fakeAPI) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeAPI) find(method, prefix string) *recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.reqs {
		if f.reqs[i].Method == method && strings.HasPrefix(f.reqs[i].URI, prefix) {
			return &f.reqs[i]
		}
	}
	return nil
}

// testEnv is an App wired to buffers, a temp config and a temp work dir.
type testEnv struct {
	app           *App
	out, errb     *bytes.Buffer
	cfgPath, wdir string
	api           *fakeAPI
	env           map[string]string
}

// newEnv returns an env logged in to a fake API through DOCK_API_URL/KEY.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	api := newFakeAPI(t)
	te := &testEnv{out: &bytes.Buffer{}, errb: &bytes.Buffer{}, api: api,
		cfgPath: filepath.Join(t.TempDir(), "cfg", "config.toml"), wdir: t.TempDir(),
		env: map[string]string{"DOCK_API_URL": api.srv.URL, "DOCK_API_KEY": "dock_testkey"}}
	te.app = &App{Stdin: strings.NewReader(""), Stdout: te.out, Stderr: te.errb, ConfigPath: te.cfgPath, WorkDir: te.wdir,
		Getenv: func(k string) string { return te.env[k] }}
	return te
}

func (te *testEnv) run(args ...string) int {
	te.out.Reset()
	te.errb.Reset()
	return te.app.Run(context.Background(), args)
}

func (te *testEnv) writeDockToml(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(te.wdir, "dock.toml"), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
