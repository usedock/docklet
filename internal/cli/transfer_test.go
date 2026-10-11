package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/usedock/docklet/internal/dockyard"
)

const (
	labKey    = "dock_labkey_secret"
	remoteID  = "55555555-5555-4555-8555-555555555555"
	importID  = "imp_1"
	exportID  = "exp_1"
	diskBytes = "raw-disk-bytes-0123456789-0123456789-0123456789-0123456789"
)

func sha256Of(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakePlane is an httptest fake of the push and pull routes of a control plane.
type fakePlane struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	mode string // direct or proxy

	importBody map[string]any
	parts      map[int][]byte
	partAuth   map[string][]string // path -> Authorization headers seen
	partFail   int                 // the first N part PUTs answer 500
	partStatus int                 // every part PUT answers this status when set
	partPuts   int
	completed  []donePart
	dockState  string

	// pull
	exportState   map[string]any
	exportPosts   int
	exportErr     *apiErr // POST /export answers this error
	payload       []byte
	cutFirst      bool // the first download is cut half way
	downloads     []string
	downloadsAuth []string
	flakyURL      bool // first GET of the export carries a url that 403s
}

func newFakePlane(t *testing.T, mode string) *fakePlane {
	p := &fakePlane{t: t, mode: mode, parts: map[int][]byte{}, partAuth: map[string][]string{}, dockState: "ready"}
	mux := http.NewServeMux()
	env := func(w http.ResponseWriter, status int, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
	}
	fail := func(w http.ResponseWriter, status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "code": code, "message": msg})
	}
	mux.HandleFunc("/v1/organizations", func(w http.ResponseWriter, r *http.Request) { env(w, 200, []any{}) })
	mux.HandleFunc("/v1/docks/import", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&p.importBody)
		plan := map[string]any{"mode": p.mode, "partSize": 8}
		if p.mode == "direct" {
			size := int(p.importBody["sizeBytes"].(float64))
			var parts []any
			for i := 1; i <= (size+7)/8; i++ {
				parts = append(parts, map[string]any{"number": i, "url": p.srv.URL + "/s3/part/" + strconv.Itoa(i)})
			}
			plan["parts"] = parts
		}
		env(w, 201, map[string]any{"importId": importID, "upload": plan})
	})
	put := func(w http.ResponseWriter, r *http.Request, n int) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.partPuts++
		p.partAuth[r.URL.Path] = append(p.partAuth[r.URL.Path], r.Header.Get("Authorization"))
		if p.partStatus != 0 {
			fail(w, p.partStatus, "forbidden", "expired")
			return
		}
		if p.partFail > 0 {
			p.partFail--
			fail(w, 500, "boom", "try later")
			return
		}
		p.parts[n] = body
		w.Header().Set("ETag", `"etag-`+strconv.Itoa(n)+`"`)
		env(w, 200, map[string]any{})
	}
	mux.HandleFunc("/s3/part/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/s3/part/"))
		put(w, r, n)
	})
	mux.HandleFunc("/v1/docks/import/"+importID+"/parts/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/v1/docks/import/"+importID+"/parts/"))
		put(w, r, n)
	})
	mux.HandleFunc("/v1/docks/import/"+importID+"/complete", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Parts []donePart `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		p.mu.Lock()
		p.completed = b.Parts
		p.mu.Unlock()
		env(w, 202, map[string]any{"id": remoteID, "name": "web", "state": "creating"})
	})
	mux.HandleFunc("/v1/docks/"+remoteID, func(w http.ResponseWriter, r *http.Request) {
		env(w, 200, map[string]any{"id": remoteID, "name": "web", "state": p.dockState, "image": "ubuntu-24.04"})
	})
	mux.HandleFunc("/v1/docks/"+remoteID+"/export", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.exportPosts++
		e := p.exportErr
		p.mu.Unlock()
		if e != nil {
			fail(w, 409, e.code, e.msg)
			return
		}
		env(w, 202, map[string]any{"exportId": exportID})
	})
	mux.HandleFunc("/v1/docks/exports/"+exportID, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		st := map[string]any{}
		for k, v := range p.exportState {
			st[k] = v
		}
		if st["state"] == "ready" {
			st["url"] = p.srv.URL + "/dl/export"
			if p.flakyURL {
				p.flakyURL = false
				st["url"] = p.srv.URL + "/dl/expired"
			}
		}
		env(w, 200, st)
	})
	mux.HandleFunc("/dl/expired", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.downloads = append(p.downloads, "expired")
		p.mu.Unlock()
		w.WriteHeader(403)
	})
	mux.HandleFunc("/dl/export", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.downloads = append(p.downloads, r.Header.Get("Range"))
		p.downloadsAuth = append(p.downloadsAuth, r.Header.Get("Authorization"))
		cut := p.cutFirst
		p.cutFirst = false
		p.mu.Unlock()
		if cut {
			w.Header().Set("Content-Length", strconv.Itoa(len(p.payload)))
			_, _ = w.Write(p.payload[:len(p.payload)/2])
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "export", time.Time{}, bytes.NewReader(p.payload))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// readyExport makes the plane offer payload as a finished export.
func (p *fakePlane) readyExport(payload []byte, base string) {
	p.payload = payload
	p.exportState = map[string]any{"state": "ready", "image": "ubuntu-24.04", "cpus": 3, "memoryMiB": 768, "diskGiB": 4,
		"sizeBytes": len(payload), "sha256": sha256Of(payload), "baseImage": base}
}

// transferEnv is a local env plus a context "lab" pointing at the fake plane.
func transferEnv(t *testing.T, mode string) (*testEnv, *fakePlane) {
	t.Helper()
	te, _ := newLocalEnv(t)
	p := newFakePlane(t, mode)
	cfg := &Config{Contexts: map[string]Context{"lab": {URL: p.srv.URL, APIKey: labKey, Workspace: wsID}}}
	if err := cfg.Save(te.cfgPath); err != nil {
		t.Fatal(err)
	}
	te.app.xferBackoff, te.app.xferPoll = time.Millisecond, time.Millisecond
	return te, p
}

func tmpEntries(t *testing.T, te *testEnv) []os.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(te.app.Engine.Dir, "tmp"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return ents
}

func unzstd(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func (p *fakePlane) uploaded() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	var all []byte
	for i := 1; i <= len(p.parts); i++ {
		all = append(all, p.parts[i]...)
	}
	return all
}

func stoppedLocalDock(t *testing.T, te *testEnv) (id string) {
	t.Helper()
	mustExit(t, te, 0, "new", "web", "--json")
	id = jsonOut(t, te).(map[string]any)["id"].(string)
	mustExit(t, te, 0, "stop", "web")
	return id
}

func TestPushDirectUpload(t *testing.T) {
	te, p := transferEnv(t, "direct")
	id := stoppedLocalDock(t, te)
	tokenRaw, _ := os.ReadFile(filepath.Join(te.app.Engine.Dir, "docks", id, "token"))
	if len(tokenRaw) == 0 {
		t.Fatal("the dock has no token file to protect")
	}

	mustExit(t, te, 0, "push", "web", "--to", "lab", "--json")
	if jsonOut(t, te).(map[string]any)["id"] != remoteID {
		t.Fatalf("push --json must print the remote dock: %s", te.out)
	}
	if strings.Contains(te.errb.String(), "uploaded") {
		t.Fatalf("--json must keep progress off stderr: %q", te.errb)
	}
	b := p.importBody
	body, _ := json.Marshal(b)
	if b["workspaceId"] != wsID || b["name"] != "web" || b["image"] != "ubuntu-24.04" || b["cpus"] != 2.0 || b["diskGiB"] != 10.0 {
		t.Fatalf("import body %s", body)
	}
	got := p.uploaded()
	if len(p.parts) < 2 {
		t.Fatalf("the upload must be split into parts, got %d", len(p.parts))
	}
	if sha256Of(got) != b["sha256"] || float64(len(got)) != b["sizeBytes"] {
		t.Fatalf("size/sha256 in the import request must describe the uploaded bytes")
	}
	if unzstd(t, got) != "overlay" {
		t.Fatalf("uploaded disk %q", unzstd(t, got))
	}
	if len(p.completed) != len(p.parts) || p.completed[0].ETag != `"etag-1"` {
		t.Fatalf("complete parts %+v", p.completed)
	}
	// Direct PUTs go to presigned URLs: no API key, and the token never leaves the machine.
	for path, auths := range p.partAuth {
		for _, a := range auths {
			if a != "" {
				t.Fatalf("a presigned PUT to %s carried Authorization %q", path, a)
			}
		}
	}
	if bytes.Contains(got, tokenRaw) || strings.Contains(string(body), string(tokenRaw)) {
		t.Fatal("the local token must never be sent")
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
	// The local dock is kept without --rm.
	if _, err := te.app.Engine.Lookup("web"); err != nil {
		t.Fatal(err)
	}
}

func TestPushProxyUploadUsesTheAPIKey(t *testing.T) {
	te, p := transferEnv(t, "proxy")
	stoppedLocalDock(t, te)
	mustExit(t, te, 0, "push", "web", "--to", "lab", "--name", "web2", "--rm")
	if p.importBody["name"] != "web2" {
		t.Fatalf("--name: %v", p.importBody)
	}
	if !strings.Contains(te.out.String(), "pushed web to lab") {
		t.Fatalf("out %q", te.out)
	}
	if !strings.Contains(te.errb.String(), "uploaded") {
		t.Fatalf("progress belongs on stderr: %q", te.errb)
	}
	if len(p.partAuth) == 0 {
		t.Fatal("no part was uploaded")
	}
	for path, auths := range p.partAuth {
		if !strings.Contains(path, "/parts/") {
			t.Fatalf("unexpected path %s", path)
		}
		for _, a := range auths {
			if a != "Bearer "+labKey {
				t.Fatalf("proxy PUT Authorization %q", a)
			}
		}
	}
	if unzstd(t, p.uploaded()) != "overlay" {
		t.Fatal("uploaded disk differs")
	}
	if _, err := te.app.Engine.Lookup("web"); err == nil {
		t.Fatal("--rm must delete the local dock after a successful push")
	}
}

func TestPushRetriesPartsWithBackoff(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	p.partFail = 3
	mustExit(t, te, 0, "push", "web", "--to", "lab")
	if p.partPuts != 3+len(p.parts) {
		t.Fatalf("puts %d, parts %d", p.partPuts, len(p.parts))
	}

	// A 403 (expired presigned url) is not retried.
	te, p = transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	p.partStatus = 403
	mustExit(t, te, 1, "push", "web", "--to", "lab")
	if p.partPuts != 1 || !strings.Contains(te.errb.String(), "upload_failed") {
		t.Fatalf("puts %d stderr %q", p.partPuts, te.errb)
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatal("temp files must be cleaned on failure")
	}

	// A part that keeps failing gives up after the attempt limit.
	te, p = transferEnv(t, "proxy")
	stoppedLocalDock(t, te)
	p.partFail = 1000
	mustExit(t, te, 1, "push", "web", "--to", "lab")
	if p.partPuts != partAttempts {
		t.Fatalf("puts %d, want %d", p.partPuts, partAttempts)
	}
}

func TestPushRefusesARunningDockUnlessStopped(t *testing.T) {
	te, p := transferEnv(t, "direct")
	mustExit(t, te, 0, "new", "web")
	mustExit(t, te, 1, "push", "web", "--to", "lab")
	if !strings.Contains(te.errb.String(), "dock_running") || p.importBody != nil {
		t.Fatalf("stderr %q import %v", te.errb, p.importBody)
	}
	mustExit(t, te, 0, "push", "web", "--to", "lab", "--stop")
	if p.importBody == nil {
		t.Fatal("--stop must push")
	}
}

func TestPushRejectsLocalAndUnknownContexts(t *testing.T) {
	te, _ := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	mustExit(t, te, 1, "push", "web", "--to", "local")
	mustExit(t, te, 1, "push", "web", "--to", "nope")
	mustExit(t, te, 2, "push", "web")
	mustExit(t, te, 1, "push", "ghost", "--to", "lab")
}

func TestPushMapsContractErrors(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	p.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"ok":false,"code":"runtime_unsupported","message":"x"}`))
	})
	mustExit(t, te, 1, "push", "web", "--to", "lab", "--workspace", wsID)
	if !strings.Contains(te.errb.String(), "runtime_unsupported") || !strings.Contains(te.errb.String(), "microVM host") {
		t.Fatalf("stderr %q", te.errb)
	}
}

func TestPullHappyPath(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "back", "--json")
	d := jsonOut(t, te).(map[string]any)
	if d["name"] != "back" || d["state"] != "archived" || d["cpus"] != 3.0 || d["memoryMiB"] != 768.0 || d["diskGiB"] != 4.0 || d["image"] != "ubuntu-24.04" {
		t.Fatalf("dock %v", d)
	}
	rec, err := te.app.Engine.Lookup("back")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(te.app.Engine.Dir, "docks", rec.ID)
	disk, _ := os.ReadFile(filepath.Join(dir, "disk.qcow2"))
	if string(disk) != diskBytes {
		t.Fatalf("disk %q", disk)
	}
	for _, f := range []string{dockyard.FileKernel, dockyard.FileInitrd} {
		if _, err := os.Stat(filepath.Join(rec.ImageDir, f)); err != nil {
			t.Fatalf("the Dockyard %s of the base image must be cached: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
		t.Fatal("a pulled dock must not be running")
	}
	for _, a := range p.downloadsAuth {
		if a != "" {
			t.Fatal("the presigned download must not carry the API key")
		}
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
}

func TestPullResumesAnInterruptedDownload(t *testing.T) {
	te, p := transferEnv(t, "direct")
	want := strings.Repeat(diskBytes, 50)
	p.readyExport(zstdOf(t, want), "ubuntu-24.04")
	p.cutFirst = true
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "back")
	if len(p.downloads) != 2 || p.downloads[0] != "" || !strings.HasPrefix(p.downloads[1], "bytes=") || p.downloads[1] == "bytes=0-" {
		t.Fatalf("the second request must resume with Range, got %q", p.downloads)
	}
	rec, _ := te.app.Engine.Lookup("back")
	disk, _ := os.ReadFile(filepath.Join(te.app.Engine.Dir, "docks", rec.ID, "disk.qcow2"))
	if string(disk) != want {
		t.Fatal("resumed download produced a different disk")
	}
}

func TestPullFetchesAFreshURLAfterAnExpiredOne(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	p.flakyURL = true
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "back")
	if len(p.downloads) != 2 || p.downloads[0] != "expired" {
		t.Fatalf("downloads %q", p.downloads)
	}
}

func TestPullRefusesAChecksumMismatch(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	p.exportState["sha256"] = strings.Repeat("0", 64)
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "checksum_mismatch") {
		t.Fatalf("stderr %q", te.errb)
	}
	if _, err := te.app.Engine.Lookup("back"); err == nil {
		t.Fatal("a corrupt download must not register a dock")
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatalf("temp files left behind: %v", tmpEntries(t, te))
	}
}

func TestPullRefusesABodyLargerThanTheExport(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	p.exportState["sizeBytes"] = len(p.payload) - 3
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "download_failed") {
		t.Fatalf("stderr %q", te.errb)
	}
}

func TestPullEnforcesTheDecompressionCap(t *testing.T) {
	te, p := transferEnv(t, "direct")
	// 2 GiB of zeros compresses to almost nothing but exceeds the 1 GiB disk the export declares.
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	zero := make([]byte, 1<<20)
	for range 2048 {
		_, _ = zw.Write(zero)
	}
	zw.Close()
	p.readyExport(buf.Bytes(), "ubuntu-24.04")
	p.exportState["diskGiB"] = 1
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "decompress_failed") {
		t.Fatalf("stderr %q", te.errb)
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatal("temp files left behind")
	}
}

func TestPullBaseUnsupported(t *testing.T) {
	// The host reports it through the export state.
	te, p := transferEnv(t, "direct")
	p.exportState = map[string]any{"state": "failed", "error": "export_base_unsupported"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "export_base_unsupported") || !strings.Contains(te.errb.String(), "Dockyard image") {
		t.Fatalf("stderr %q", te.errb)
	}
	// Or the route refuses it up front.
	te, p = transferEnv(t, "direct")
	p.exportErr = &apiErr{"export_base_unsupported", "base"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "Dockyard image") {
		t.Fatalf("stderr %q", te.errb)
	}
	// A failed export with another reason is shown as is.
	te, p = transferEnv(t, "direct")
	p.exportState = map[string]any{"state": "failed", "error": "host went away"}
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "export_failed: host went away") {
		t.Fatalf("stderr %q", te.errb)
	}
	// A ready export without a Dockyard base is refused before any download.
	te, p = transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if len(p.downloads) != 0 || !strings.Contains(te.errb.String(), "export_base_unsupported") {
		t.Fatalf("downloads %v stderr %q", p.downloads, te.errb)
	}
}

func TestPullNameCollisionFailsBeforeExporting(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 0, "new", "taken")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "taken")
	if !strings.Contains(te.errb.String(), "dock_name_taken") || p.exportPosts != 0 {
		t.Fatalf("stderr %q exports %d", te.errb, p.exportPosts)
	}
	// The default name is the remote dock's name ("web").
	mustExit(t, te, 0, "pull", remoteID, "--from", "lab")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab")
	if !strings.Contains(te.errb.String(), "dock_name_taken") {
		t.Fatalf("stderr %q", te.errb)
	}
}
