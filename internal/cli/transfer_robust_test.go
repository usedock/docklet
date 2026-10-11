package cli

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// dlRig is a fake plane whose /dl/export route is replaced by h, plus what a.download needs.
type dlRig struct {
	te      *testEnv
	p       *fakePlane
	cl      *Client
	info    exportInfo
	payload []byte
	part    string

	mu   sync.Mutex
	reqs []*http.Request
}

func newDLRig(t *testing.T, size int, h func(r *dlRig, w http.ResponseWriter, req *http.Request)) *dlRig {
	t.Helper()
	te, p := transferEnv(t, "direct")
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte('a' + i%23)
	}
	p.readyExport(payload, "ubuntu-24.04")
	r := &dlRig{te: te, p: p, payload: payload, part: filepath.Join(t.TempDir(), "export.zst")}
	r.cl = &Client{BaseURL: p.srv.URL, Key: labKey, HTTP: p.srv.Client()}
	r.info = exportInfo{URL: p.srv.URL + "/dl/export", SizeBytes: int64(size), SHA256: sha256Of(payload)}
	mux := p.srv.Config.Handler
	p.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/dl/export" {
			mux.ServeHTTP(w, req)
			return
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, req.Clone(context.Background()))
		r.mu.Unlock()
		h(r, w, req)
	})
	return r
}

func (r *dlRig) download() error {
	return r.te.app.download(context.Background(), r.cl, exportID, r.info, r.part, true)
}

func (r *dlRig) ranges() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.reqs {
		out = append(out, q.Header.Get("Range"))
	}
	return out
}

func (r *dlRig) got(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(r.part)
	if err != nil || !bytes.Equal(b, r.payload) {
		t.Fatalf("the downloaded file differs from the export (err %v, %d bytes)", err, len(b))
	}
}

// cutAfter writes n bytes of the body of a response and drops the connection.
func cutAfter(w http.ResponseWriter, body []byte, total int, n int) {
	w.Header().Set("Content-Length", strconv.Itoa(total))
	_, _ = w.Write(body[:n])
	_ = http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

func startOf(rng string) int {
	n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
	return n
}

func TestBackoffIsJitteredAndCapped(t *testing.T) {
	a := &App{xferBackoff: time.Second}
	for n := 1; n <= 40; n++ {
		d := min(time.Second<<min(n-1, 10), maxBackoff)
		seen := map[time.Duration]bool{}
		for range 300 {
			got := a.backoffFor(n)
			if got < d/2 || got > d {
				t.Fatalf("retry %d waited %s, want within [%s, %s]", n, got, d/2, d)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Fatalf("retry %d: backoff has no jitter", n)
		}
	}
	// With the test backoff of 1ms nothing waits long.
	if got := (&App{xferBackoff: time.Millisecond}).backoffFor(3); got > 4*time.Millisecond {
		t.Fatalf("got %s", got)
	}
}

func TestDownloadBudgetResetsWhileProgressing(t *testing.T) {
	// Every response moves 10 bytes and then drops: 12 attempts in all, twice the budget.
	r := newDLRig(t, 120, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
		start := startOf(req.Header.Get("Range"))
		end := min(start+10, len(r.payload))
		if start > 0 {
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(r.payload)-1)+"/"+strconv.Itoa(len(r.payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(r.payload[start:end])
		if end < len(r.payload) {
			_ = http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		}
	})
	if err := r.download(); err != nil {
		t.Fatalf("a download that keeps moving must not give up: %v", err)
	}
	if len(r.reqs) <= downloadAttempts {
		t.Fatalf("only %d requests; the test must exceed the %d-attempt budget", len(r.reqs), downloadAttempts)
	}
	r.got(t)
}

func TestDownloadGivesUpWithoutProgress(t *testing.T) {
	r := newDLRig(t, 50, func(r *dlRig, w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	err := r.download()
	if err == nil || !strings.Contains(err.Error(), "download_failed") {
		t.Fatalf("got %v", err)
	}
	if len(r.reqs) != downloadAttempts {
		t.Fatalf("%d requests, want %d", len(r.reqs), downloadAttempts)
	}
}

func TestResumeSendsIfRangeAndChecksContentRange(t *testing.T) {
	serve := func(etag string) func(*dlRig, http.ResponseWriter, *http.Request) {
		return func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			w.Header().Set("ETag", etag)
			if req.Header.Get("Range") == "" && len(r.reqs) == 1 {
				cutAfter(w, r.payload, len(r.payload), len(r.payload)/2)
			}
			http.ServeContent(w, req, "export", time.Time{}, bytes.NewReader(r.payload))
		}
	}
	t.Run("if-range carries the first etag", func(t *testing.T) {
		r := newDLRig(t, 100, serve(`"v1"`))
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if len(r.reqs) != 2 || r.reqs[1].Header.Get("If-Range") != `"v1"` || r.reqs[0].Header.Get("If-Range") != "" {
			t.Fatalf("requests %q", r.ranges())
		}
		r.got(t)
	})
	t.Run("last-modified is used without an etag", func(t *testing.T) {
		const lm = "Wed, 21 Oct 2015 07:28:00 GMT"
		r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Last-Modified", lm)
			if req.Header.Get("Range") == "" {
				cutAfter(w, r.payload, len(r.payload), 40)
			}
			start := startOf(req.Header.Get("Range"))
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-99/100")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(r.payload[start:])
		})
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if r.reqs[1].Header.Get("If-Range") != lm {
			t.Fatalf("If-Range %q", r.reqs[1].Header.Get("If-Range"))
		}
		r.got(t)
	})
	t.Run("a changed object is fetched whole", func(t *testing.T) {
		n := 0
		r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			n++
			w.Header().Set("ETag", `"v`+strconv.Itoa(n)+`"`) // a new version on every request
			if n == 1 {
				cutAfter(w, r.payload, len(r.payload), 40)
			}
			http.ServeContent(w, req, "export", time.Time{}, bytes.NewReader(r.payload))
		})
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if len(r.reqs) != 2 {
			t.Fatalf("requests %q", r.ranges())
		}
		r.got(t)
	})
	t.Run("a 206 for another range restarts from zero", func(t *testing.T) {
		r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			switch len(r.reqs) {
			case 1:
				cutAfter(w, r.payload, len(r.payload), 40)
			case 2: // answers from byte 0 although 40 were asked for
				w.Header().Set("Content-Range", "bytes 0-99/100")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(r.payload[40:])
			default:
				_, _ = w.Write(r.payload)
			}
		})
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if got := r.ranges(); len(got) != 3 || got[1] != "bytes=40-" || got[2] != "" {
			t.Fatalf("requests %q: the third must start from zero", got)
		}
		r.got(t)
	})
	t.Run("a 206 without Content-Range restarts from zero", func(t *testing.T) {
		r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			switch len(r.reqs) {
			case 1:
				cutAfter(w, r.payload, len(r.payload), 40)
			case 2:
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(r.payload[40:])
			default:
				_, _ = w.Write(r.payload)
			}
		})
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if got := r.ranges(); len(got) != 3 || got[2] != "" {
			t.Fatalf("requests %q", got)
		}
		r.got(t)
	})
	t.Run("a 206 of a changed object restarts from zero", func(t *testing.T) {
		r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
			switch len(r.reqs) {
			case 1:
				w.Header().Set("ETag", `"v1"`)
				cutAfter(w, r.payload, len(r.payload), 40)
			case 2: // ignores If-Range and answers a correct range of a different version
				w.Header().Set("ETag", `"v2"`)
				w.Header().Set("Content-Range", "bytes 40-99/100")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(r.payload[40:])
			default:
				w.Header().Set("ETag", `"v2"`)
				_, _ = w.Write(r.payload)
			}
		})
		if err := r.download(); err != nil {
			t.Fatal(err)
		}
		if got := r.ranges(); len(got) != 3 || got[2] != "" {
			t.Fatalf("requests %q", got)
		}
		r.got(t)
	})
}

func TestResumePastTheEndRestartsOn416(t *testing.T) {
	r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
		switch len(r.reqs) {
		case 1:
			cutAfter(w, r.payload, len(r.payload), 60)
		case 2:
			w.Header().Set("Content-Range", "bytes */20")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		default:
			_, _ = w.Write(r.payload)
		}
	})
	if err := r.download(); err != nil {
		t.Fatalf("a 416 must restart the download: %v", err)
	}
	if got := r.ranges(); len(got) != 3 || got[2] != "" {
		t.Fatalf("requests %q", got)
	}
	r.got(t)
}

func TestParseContentRangeForms(t *testing.T) {
	h := func(v string) http.Header { return http.Header{"Content-Range": {v}} }
	for _, tc := range []struct {
		cr   string
		have int64
		bad  bool
	}{
		{"bytes 40-99/100", 40, false},
		{"bytes 40-99/*", 40, false},
		{"bytes 0-99/100", 40, true},
		{"bytes 40-98/100", 40, true},
		{"bytes 40-99/101", 40, true},
		{"", 40, true},
		{"items 40-99/100", 40, true},
	} {
		if got := badContentRange(h(tc.cr), tc.have, 100) != ""; got != tc.bad {
			t.Errorf("%q at %d: bad=%v, want %v", tc.cr, tc.have, got, tc.bad)
		}
	}
	if parseTotalOnly("bytes */20") != 20 || parseTotalOnly("bytes 1-2/3") != -1 {
		t.Fatal("416 Content-Range is not parsed")
	}
}

func TestStalledDownloadIsRetried(t *testing.T) {
	r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
		if len(r.reqs) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(r.payload)))
			_, _ = w.Write(r.payload[:40])
			_ = http.NewResponseController(w).Flush()
			select { // a connection that stays open and silent
			case <-req.Context().Done():
			case <-time.After(20 * time.Second):
			}
			return
		}
		http.ServeContent(w, req, "export", time.Time{}, bytes.NewReader(r.payload))
	})
	r.te.app.xferIdle = 300 * time.Millisecond
	start := time.Now()
	if err := r.download(); err != nil {
		t.Fatalf("a stalled connection must fail the attempt and be retried: %v", err)
	}
	if time.Since(start) > 10*time.Second || len(r.reqs) != 2 || r.ranges()[1] != "bytes=40-" {
		t.Fatalf("requests %q after %s", r.ranges(), time.Since(start))
	}
	r.got(t)
}

func TestSlowButMovingDownloadIsNotCut(t *testing.T) {
	// Each write comes before the idle limit, so the transfer outlasts it overall.
	r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
		for i := 0; i < len(r.payload); i += 10 {
			_, _ = w.Write(r.payload[i : i+10])
			_ = http.NewResponseController(w).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	})
	r.te.app.xferIdle = 600 * time.Millisecond
	if err := r.download(); err != nil {
		t.Fatalf("the idle timeout must not be a total timeout: %v", err)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("requests %q", r.ranges())
	}
	r.got(t)
}

func TestHeaderWaitOutlastsTheServersPartQueue(t *testing.T) {
	if defaultHeaderWait <= 60*time.Second || defaultIdle <= 0 {
		t.Fatalf("the header wait (%s) must exceed the 60s the control plane may queue a part", defaultHeaderWait)
	}
}

// wrapPlane lets h intercept paths with the given prefix; it returns true when it handled it.
func wrapPlane(p *fakePlane, prefix string, h func(w http.ResponseWriter, r *http.Request) bool) {
	mux := p.srv.Config.Handler
	p.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) && h(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func TestPushRetriesPartUploadBusyBeyondTheUsualBudget(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	var mu sync.Mutex
	busy := 0
	wrapPlane(p, "/s3/part/", func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if busy >= partAttempts+3 {
			return false
		}
		busy++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"code":"part_upload_busy","message":"busy"}`))
		return true
	})
	mustExit(t, te, 0, "push", "web", "--to", "lab")
	if busy != partAttempts+3 || len(p.parts) == 0 {
		t.Fatalf("busy %d, parts %d", busy, len(p.parts))
	}

	// A plain 429 keeps the normal budget.
	te, p = transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	p.partStatus = http.StatusTooManyRequests
	mustExit(t, te, 1, "push", "web", "--to", "lab")
	if p.partPuts != partAttempts {
		t.Fatalf("puts %d, want %d", p.partPuts, partAttempts)
	}
}

func TestStalledPartUploadIsRetried(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	te.app.xferHeaderWait = 300 * time.Millisecond
	var mu sync.Mutex
	stalled := false
	wrapPlane(p, "/s3/part/", func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		first := !stalled
		stalled = true
		mu.Unlock()
		if !first {
			return false
		}
		_, _ = readAll(r) // reading the body lets the server notice the client hanging up
		// Never answers.
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return true
	})
	mustExit(t, te, 0, "push", "web", "--to", "lab")
	if len(p.parts) == 0 {
		t.Fatal("the push must finish after the stalled attempt is retried")
	}
}

func TestSlowPartAnswerWithinTheHeaderWaitSucceeds(t *testing.T) {
	te, p := transferEnv(t, "direct")
	stoppedLocalDock(t, te)
	// The idle limit is far below the delay: only the header wait applies after the body is sent.
	te.app.xferIdle, te.app.xferHeaderWait = 100*time.Millisecond, 10*time.Second
	var mu sync.Mutex
	slow := false
	wrapPlane(p, "/s3/part/", func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		first := !slow
		slow = true
		mu.Unlock()
		if first {
			_, _ = readAll(r)
			time.Sleep(600 * time.Millisecond)
		}
		return false
	})
	mustExit(t, te, 0, "push", "web", "--to", "lab")
	if p.partPuts != len(p.parts) {
		t.Fatalf("puts %d for %d parts: the slow answer must not cause a retry", p.partPuts, len(p.parts))
	}
}

func readAll(r *http.Request) (int64, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(r.Body)
	// Hand the body back so the fake plane can read it again.
	r.Body = nopCloser{bytes.NewReader(buf.Bytes())}
	return n, err
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func TestPushChecksFreeSpace(t *testing.T) {
	te, p := transferEnv(t, "direct")
	// A huge virtual disk that holds a few bytes.
	mustExit(t, te, 0, "new", "web", "--disk", "100000000")
	mustExit(t, te, 0, "stop", "web")
	if runtime.GOOS != "windows" {
		// The flattened raw is sparse: only the data needs room, so the push goes through.
		mustExit(t, te, 0, "push", "web", "--to", "lab")
		return
	}
	mustExit(t, te, 1, "push", "web", "--to", "lab")
	if !strings.Contains(te.errb.String(), "insufficient_space") || !strings.Contains(te.errb.String(), "docklet#76") || p.importBody != nil {
		t.Fatalf("stderr %q import %v", te.errb, p.importBody)
	}
	if len(tmpEntries(t, te)) != 0 {
		t.Fatal("temp files must be cleaned")
	}
}

func TestPullChecksFreeSpaceBeforeDownloading(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	p.exportState["diskGiB"] = 100000000
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "back")
	if !strings.Contains(te.errb.String(), "insufficient_space") || len(p.downloads) != 0 {
		t.Fatalf("stderr %q downloads %v", te.errb, p.downloads)
	}
}

func TestPullRejectsAnInvalidNameBeforeExporting(t *testing.T) {
	te, p := transferEnv(t, "direct")
	p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
	mustExit(t, te, 1, "pull", remoteID, "--from", "lab", "--name", "bad name!")
	if !strings.Contains(te.errb.String(), "invalid_argument") || p.exportPosts != 0 || len(p.downloads) != 0 {
		t.Fatalf("stderr %q exports %d downloads %v", te.errb, p.exportPosts, p.downloads)
	}
}

func TestPushAndPullSweepStaleTransferDirs(t *testing.T) {
	for _, cmd := range []string{"pull", "push"} {
		t.Run(cmd, func(t *testing.T) {
			te, p := transferEnv(t, "direct")
			p.readyExport(zstdOf(t, diskBytes), "ubuntu-24.04")
			stale := filepath.Join(te.app.Engine.Dir, "tmp", "xfer-2147483646-old")
			if err := os.MkdirAll(stale, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stale, "export.zst"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			for _, f := range []string{filepath.Join(stale, "export.zst"), stale} {
				if err := os.Chtimes(f, old, old); err != nil {
					t.Fatal(err)
				}
			}
			if cmd == "pull" {
				mustExit(t, te, 0, "pull", remoteID, "--from", "lab", "--name", "back")
			} else {
				stoppedLocalDock(t, te)
				mustExit(t, te, 0, "push", "web", "--to", "lab")
			}
			if _, err := os.Stat(stale); err == nil {
				t.Fatal("a stale transfer directory must be removed at startup")
			}
		})
	}
}

func TestDownloadRestartsDoNotResetTheBudget(t *testing.T) {
	// From zero the body always drops at 40 bytes; every resume answers the wrong range and
	// restarts. Regrowing the same 40 bytes is not progress, so the budget must run out.
	r := newDLRig(t, 100, func(r *dlRig, w http.ResponseWriter, req *http.Request) {
		if len(r.reqs) > 4*downloadAttempts {
			_, _ = w.Write(r.payload) // let a looping client finish so the test reports it
			return
		}
		if req.Header.Get("Range") == "" {
			cutAfter(w, r.payload, len(r.payload), 40)
		}
		w.Header().Set("Content-Range", "bytes 0-99/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(r.payload)
	})
	err := r.download()
	if err == nil || !strings.Contains(err.Error(), "download_failed") {
		t.Fatalf("a download that keeps restarting must give up, got %v after %d requests", err, len(r.reqs))
	}
	if len(r.reqs) > 2*downloadAttempts+1 {
		t.Fatalf("%d requests: restarts must not refill the budget", len(r.reqs))
	}
}
