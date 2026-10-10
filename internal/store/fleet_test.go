package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method      string
	url         string
	header      http.Header
	body        []byte
	contentType string
}

// fakeTransport records object-proxy calls and answers them from a handler.
type fakeTransport struct {
	mu      sync.Mutex
	calls   []recorded
	handler func(recorded) (*http.Response, error)
	// handlerServesURL lets a test answer transfer-url requests itself; without
	// it they get the "proxy" answer, which is what a control plane that cannot
	// presign returns and what the older proxy tests expect.
	handlerServesURL bool
	// servedURL is prepended to the recorded call count so url requests stay
	// out of the calls older tests assert on.
	urlCalls int
}

func (f *fakeTransport) Do(req *http.Request) (*http.Response, error) {
	if !f.handlerServesURL && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/url") {
		f.mu.Lock()
		f.urlCalls++
		f.mu.Unlock()
		return okJSON(`{"ok":true,"data":{"mode":"proxy","url":"","expiresInSeconds":0}}`), nil
	}
	var body []byte
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		// The real transport closes the request body it is handed, even on
		// errors. A double that does not will hide a retry seeking a closed
		// source, so this closes it too.
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = raw
	}
	call := recorded{
		method:      req.Method,
		url:         req.URL.String(),
		header:      req.Header.Clone(),
		body:        body,
		contentType: req.Header.Get("content-type"),
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	handler := f.handler
	f.mu.Unlock()
	if handler == nil {
		return nil, fmt.Errorf("unexpected request %s %s", call.method, call.url)
	}
	return handler(call)
}

func (f *fakeTransport) snapshot() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.calls...)
}

func okJSON(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"content-type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func statusJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"content-type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newTestStore(t *testing.T, transport *fakeTransport) *FleetStore {
	t.Helper()
	s, err := DialFleet("https://api.usedock.test/", "fleet-token")
	if err != nil {
		t.Fatalf("DialFleet: %v", err)
	}
	s.client = transport
	s.ScratchDir = t.TempDir()
	s.Logf = t.Logf
	return s
}

// queryOf parses the part/parts/size triple of one upload call.
func queryOf(t *testing.T, call recorded) (part, parts int, size int64) {
	t.Helper()
	parsed, err := url.Parse(call.url)
	if err != nil {
		t.Fatalf("url %s: %v", call.url, err)
	}
	part, _ = strconv.Atoi(parsed.Query().Get("part"))
	parts, _ = strconv.Atoi(parsed.Query().Get("parts"))
	size, _ = strconv.ParseInt(parsed.Query().Get("size"), 10, 64)
	return part, parts, size
}

func TestPartCountBoundaries(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want int
	}{
		{-1, 1}, {0, 1}, {1, 1}, {maxPart - 1, 1}, {maxPart, 1},
		{maxPart + 1, 2}, {2 * maxPart, 2}, {2*maxPart + 1, 3},
	} {
		if got := partCount(tc.size); got != tc.want {
			t.Fatalf("partCount(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

func TestPutSingleSmallObjectSendsOnePart(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return okJSON(`{"ok":true,"data":{"complete":true,"size":3}}`), nil
	}
	s := newTestStore(t, transport)

	if err := s.Put(context.Background(), "s1", "manifest.json", strings.NewReader("abc"), 3); err != nil {
		t.Fatalf("Put: %v", err)
	}
	calls := transport.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.method != http.MethodPut {
		t.Fatalf("method = %s, want PUT", call.method)
	}
	if call.url != "https://api.usedock.test/v1/fleet/objects/s1/manifest.json?part=0&parts=1&size=3" {
		t.Fatalf("url = %s", call.url)
	}
	if call.contentType != "application/octet-stream" {
		t.Fatalf("content-type = %q, want raw octet-stream", call.contentType)
	}
	if call.header.Get("authorization") != "Bearer fleet-token" {
		t.Fatalf("authorization = %q", call.header.Get("authorization"))
	}
	if string(call.body) != "abc" {
		t.Fatalf("body = %q", call.body)
	}
}

func TestPutChunksInOrderAndComposesOnLastPart(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return okJSON(`{"ok":true,"data":{"complete":true,"size":null}}`), nil
	}
	s := newTestStore(t, transport)
	payload := bytes.Repeat([]byte("docklet-chunk-"), maxPart/13+1)[:maxPart]
	payload = append(payload, []byte("tail-7b")...)
	if int64(len(payload)) != maxPart+7 {
		t.Fatalf("payload = %d bytes, want %d", len(payload), maxPart+7)
	}

	if err := s.Put(context.Background(), "s2", "rootfs.ext4.zst", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	calls := transport.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 parts", len(calls))
	}
	// Every part declares the same total and part count, in ascending order,
	// and the final part is parts-1: that is what triggers compose.
	for i, call := range calls {
		part, parts, size := queryOf(t, call)
		if part != i || parts != 2 {
			t.Fatalf("call %d = part %d of %d, want part %d of 2", i, part, parts, i)
		}
		if size != int64(len(payload)) {
			t.Fatalf("call %d size = %d, want %d", i, size, len(payload))
		}
		if call.header.Get("idempotency-token") != "" {
			t.Fatalf("upload must not carry an idempotency token")
		}
	}
	if len(calls[0].body) != maxPart || len(calls[1].body) != 7 {
		t.Fatalf("part sizes = %d,%d, want %d,7", len(calls[0].body), len(calls[1].body), maxPart)
	}
	if !bytes.Equal(append(calls[0].body, calls[1].body...), payload) {
		t.Fatal("reassembled parts differ from the uploaded payload")
	}
}

func TestPutUnknownLengthSpillsAndUploadsExactSize(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return okJSON(`{"ok":true,"data":{"complete":true,"size":9}}`), nil
	}
	s := newTestStore(t, transport)

	// A stream of unknown length must still declare its true size and count.
	if err := s.Put(context.Background(), "s3", "mem", strings.NewReader("nine-byte"), -1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	calls := transport.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	part, parts, size := queryOf(t, calls[0])
	if part != 0 || parts != 1 || size != 9 {
		t.Fatalf("part %d of %d size %d, want 0 of 1 size 9", part, parts, size)
	}
	if string(calls[0].body) != "nine-byte" {
		t.Fatalf("body = %q", calls[0].body)
	}
	// The spill file is scratch, not state: it must not outlive the upload.
	entries, err := os.ReadDir(s.ScratchDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch dir holds %d files after upload", len(entries))
	}
}

func TestPutRejectsReaderLongerThanDeclaredSize(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return okJSON(`{"ok":true,"data":{"complete":true,"size":null}}`), nil
	}
	s := newTestStore(t, transport)

	err := s.Put(context.Background(), "s4", "mem", strings.NewReader("longer than declared"), 4)
	if err == nil || !strings.Contains(err.Error(), "more than the declared") {
		t.Fatalf("err = %v, want a truncation refusal", err)
	}
}

func TestGetMissingObjectReportsNotFound(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return statusJSON(http.StatusNotFound, `{"ok":false,"code":"object_not_found"}`), nil
	}
	s := newTestStore(t, transport)

	_, err := s.Get(context.Background(), "s1", "manifest.json")
	if err == nil {
		t.Fatal("missing object must be an error")
	}
	// Matches MemStore's wording so callers that treat any error as absent work.
	if !strings.Contains(err.Error(), "no such object snapshots/s1/manifest.json") {
		t.Fatalf("err = %v", err)
	}
	if got := len(transport.snapshot()); got != 1 {
		t.Fatalf("attempts = %d, want 1: a 404 is permanent", got)
	}
}

func TestGetStreamsBody(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if call.header.Get("authorization") != "Bearer fleet-token" {
			return nil, fmt.Errorf("missing bearer token")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"content-type": []string{"application/octet-stream"}},
			Body:       io.NopCloser(strings.NewReader("generation-bytes")),
		}, nil
	}
	s := newTestStore(t, transport)

	rc, err := s.Get(context.Background(), "s1", "rootfs.ext4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil || string(raw) != "generation-bytes" {
		t.Fatalf("body = %q,%v", raw, err)
	}
	if call := transport.snapshot()[0]; call.url != "https://api.usedock.test/v1/fleet/objects/s1/rootfs.ext4" {
		t.Fatalf("url = %s", call.url)
	}
}

func TestListMapsStoredPathsVerbatim(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return okJSON(`{"ok":true,"data":{"files":[{"path":"deletions.txt","size":12},{"path":"rootfs.ext4.zst","size":4096}]}}`), nil
	}
	s := newTestStore(t, transport)

	entries, err := s.List(context.Background(), "s5")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 || entries[0] != (FileEntry{Path: "deletions.txt", Size: 12}) ||
		entries[1] != (FileEntry{Path: "rootfs.ext4.zst", Size: 4096}) {
		t.Fatalf("entries = %+v", entries)
	}
	for _, entry := range entries {
		if entry.Encoding != "" {
			t.Fatalf("encoding %q inferred from a stored path", entry.Encoding)
		}
	}
}

func TestDeleteReportsDeletedCount(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if call.method != http.MethodDelete {
			return nil, fmt.Errorf("method %s, want DELETE", call.method)
		}
		return okJSON(`{"ok":true,"data":{"deleted":3}}`), nil
	}
	s := newTestStore(t, transport)

	if err := s.Delete(context.Background(), "s6"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if call := transport.snapshot()[0]; call.url != "https://api.usedock.test/v1/fleet/objects/s6" {
		t.Fatalf("url = %s", call.url)
	}
}

func TestDeleteSurfacesControlPlaneFailure(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return statusJSON(http.StatusForbidden, `{"ok":false,"code":"forbidden","message":"host not enrolled"}`), nil
	}
	s := newTestStore(t, transport)

	err := s.Delete(context.Background(), "s6")
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "host not enrolled") {
		t.Fatalf("err = %v, want the control plane's code and message", err)
	}
}

func TestRetryOnServerErrorThenSucceed(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		if len(transport.snapshot()) == 1 {
			return statusJSON(http.StatusBadGateway, `{"ok":false,"code":"upstream","message":"composing"}`), nil
		}
		return okJSON(`{"ok":true,"data":{"files":[]}}`), nil
	}
	s := newTestStore(t, transport)

	if _, err := s.List(context.Background(), "s7"); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := len(transport.snapshot()); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestPermanentClientErrorIsNotRetried(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return statusJSON(http.StatusBadRequest, `{"ok":false,"code":"validation_error","message":"bad part"}`), nil
	}
	s := newTestStore(t, transport)

	_, err := s.List(context.Background(), "s8")
	if err == nil {
		t.Fatal("a 400 must surface as an error")
	}
	if got := len(transport.snapshot()); got != 1 {
		t.Fatalf("attempts = %d, want 1 for a permanent rejection", got)
	}
}

func TestUnauthorizedKeepsRetryingAndSaysWhy(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return statusJSON(http.StatusUnauthorized, `{"ok":false,"code":"unauthorized"}`), nil
	}
	logs := make(chan string, 16)
	s := newTestStore(t, transport)
	s.Logf = func(format string, args ...any) {
		select {
		case logs <- fmt.Sprintf(format, args...):
		default:
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if _, err := s.List(ctx, "s9"); err == nil {
		t.Fatal("ctx cancellation must surface as an error")
	}
	if got := len(transport.snapshot()); got < 2 {
		t.Fatalf("attempts = %d, want a 401 retried with backoff", got)
	}
	close(logs)
	var seen string
	for line := range logs {
		if strings.Contains(line, "DOCKLET_FLEET_TOKEN") {
			seen = line
		}
	}
	if seen == "" {
		t.Fatal("a 401 must log the token hint")
	}
}

// proxy is a throwaway control plane: it buffers uploaded parts and composes
// an object only when the final part arrives, mirroring the real server.
type proxy struct {
	mu      sync.Mutex
	parts   map[string][][]byte
	objects map[string][]byte
	seen    []string
	// direct makes the transfer-url route hand back a presigned url served by
	// this same server at /store/..., standing in for the object store.
	direct  bool
	baseURL string
	// storeAuthSeen records authorization headers that reached the store path.
	// It must stay empty: the presigned url is the credential, so the fleet
	// token must never travel to the object store.
	storeAuthSeen []string
	// reSigns counts presign requests, so a test can force url rotation.
	reSigns int
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.seen = append(p.seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	p.mu.Unlock()
	// The object store is a different service with its own auth; reaching it
	// must not carry the control plane's token.
	if storeKey, ok := strings.CutPrefix(r.URL.Path, "/store/"); ok {
		p.mu.Lock()
		if h := r.Header.Get("authorization"); h != "" {
			p.storeAuthSeen = append(p.storeAuthSeen, h)
		}
		p.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.objects[storeKey] = body
			p.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			p.mu.Lock()
			body, found := p.objects[storeKey]
			p.mu.Unlock()
			if !found {
				http.Error(w, "NoSuchKey", http.StatusNotFound)
				return
			}
			w.Header().Set("content-type", "application/octet-stream")
			w.Header().Set("content-length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if r.Header.Get("authorization") != "Bearer fleet-token" {
		writeEnvelope(w, http.StatusUnauthorized, `{"ok":false,"code":"unauthorized"}`)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/fleet/objects/")
	snapshotID, name, hasName := strings.Cut(rest, "/")

	switch {
	case hasName && r.Method == http.MethodPost && strings.HasSuffix(name, "/url"):
		objectName := strings.TrimSuffix(name, "/url")
		p.mu.Lock()
		p.reSigns++
		direct, base := p.direct, p.baseURL
		p.mu.Unlock()
		if !direct {
			writeEnvelope(w, http.StatusOK, `{"ok":true,"data":{"mode":"proxy","url":"","expiresInSeconds":0}}`)
			return
		}
		target := fmt.Sprintf("%s/store/%s/%s", base, snapshotID, objectName)
		writeEnvelope(w, http.StatusOK, fmt.Sprintf(`{"ok":true,"data":{"mode":"direct","url":%q,"expiresInSeconds":900}}`, target))
	case hasName && r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		part, _ := strconv.Atoi(r.URL.Query().Get("part"))
		parts, _ := strconv.Atoi(r.URL.Query().Get("parts"))
		p.mu.Lock()
		key := snapshotID + "/" + name
		p.parts[key] = append(p.parts[key], body)
		complete := part == parts-1 && len(p.parts[key]) == parts
		if complete {
			p.objects[key] = bytes.Join(p.parts[key], nil)
			delete(p.parts, key)
		}
		p.mu.Unlock()
		writeEnvelope(w, http.StatusOK, fmt.Sprintf(`{"ok":true,"data":{"complete":%t,"size":null}}`, complete))
	case hasName && r.Method == http.MethodGet:
		p.mu.Lock()
		body, ok := p.objects[snapshotID+"/"+name]
		p.mu.Unlock()
		if !ok {
			writeEnvelope(w, http.StatusNotFound, `{"ok":false,"code":"object_not_found"}`)
			return
		}
		w.Header().Set("content-type", "application/octet-stream")
		w.Header().Set("content-length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case !hasName && r.Method == http.MethodGet:
		p.mu.Lock()
		files := []map[string]any{}
		for stored, body := range p.objects {
			if id, path, ok := strings.Cut(stored, "/"); ok && id == snapshotID {
				files = append(files, map[string]any{"path": path, "size": len(body)})
			}
		}
		p.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"files": files})
		writeEnvelope(w, http.StatusOK, `{"ok":true,"data":`+string(raw)+`}`)
	case !hasName && r.Method == http.MethodDelete:
		p.mu.Lock()
		deleted := 0
		for stored := range p.objects {
			if id, _, ok := strings.Cut(stored, "/"); ok && id == snapshotID {
				delete(p.objects, stored)
				deleted++
			}
		}
		p.mu.Unlock()
		writeEnvelope(w, http.StatusOK, fmt.Sprintf(`{"ok":true,"data":{"deleted":%d}}`, deleted))
	default:
		writeEnvelope(w, http.StatusNotFound, `{"ok":false,"code":"not_found"}`)
	}
}

func writeEnvelope(w http.ResponseWriter, status int, body string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// TestFleetStoreRoundTripOverHTTP proves the whole flow against real HTTP: a
// multi-part object arrives byte-exact, and GET/List/Delete all speak the
// proxy's shape.
func TestFleetStoreRoundTripOverHTTP(t *testing.T) {
	fake := &proxy{parts: map[string][][]byte{}, objects: map[string][]byte{}}
	server := httptest.NewServer(fake)
	defer server.Close()

	s, err := DialFleet(server.URL, "fleet-token")
	if err != nil {
		t.Fatalf("DialFleet: %v", err)
	}
	s.ScratchDir = t.TempDir()
	payload := bytes.Repeat([]byte("generation-block"), maxPart/16+1)[:maxPart]
	payload = append(payload, []byte("final-part")...)

	if err := s.Put(context.Background(), "gen-1", "rootfs.ext4", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(context.Background(), "gen-1", "rootfs.ext4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("round trip returned %d bytes, err %v", len(body), err)
	}

	entries, err := s.List(context.Background(), "gen-1")
	if err != nil || len(entries) != 1 || entries[0].Path != "rootfs.ext4" || entries[0].Size != int64(len(payload)) {
		t.Fatalf("entries = %+v, err %v", entries, err)
	}
	if err := s.Delete(context.Background(), "gen-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(context.Background(), "gen-1", "rootfs.ext4"); err == nil {
		t.Fatal("a deleted generation must be gone")
	}
}

func TestDialFleetRejectsBadConfig(t *testing.T) {
	if _, err := DialFleet("https://api.usedock.test", ""); err == nil {
		t.Fatal("a missing token must be refused")
	}
	if _, err := DialFleet("api.usedock.test", "tok"); err == nil {
		t.Fatal("a url without a scheme must be refused")
	}
}

// TestFleetStoreRoundTripDirectOverHTTP proves the bytes travel straight to the
// object store when the control plane hands back a presigned url, and that the
// proxy's part protocol is not touched in that case.
func TestFleetStoreRoundTripDirectOverHTTP(t *testing.T) {
	fake := &proxy{parts: map[string][][]byte{}, objects: map[string][]byte{}, direct: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	fake.baseURL = server.URL

	s, err := DialFleet(server.URL, "fleet-token")
	if err != nil {
		t.Fatalf("DialFleet: %v", err)
	}
	s.ScratchDir = t.TempDir()

	payload := bytes.Repeat([]byte("direct-generation-block"), maxPart/8)
	if err := s.Put(context.Background(), "gen-9", "rootfs.delta.zst", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fake.mu.Lock()
	stored := append([]byte(nil), fake.objects["gen-9/rootfs.delta.zst"]...)
	authSeen := append([]string(nil), fake.storeAuthSeen...)
	seen := append([]string(nil), fake.seen...)
	fake.mu.Unlock()

	if !bytes.Equal(stored, payload) {
		t.Fatalf("store holds %d bytes, want %d: the direct upload did not arrive intact", len(stored), len(payload))
	}
	if len(authSeen) != 0 {
		t.Fatalf("the fleet token reached the object store: %v", authSeen)
	}
	for _, call := range seen {
		if strings.Contains(call, "part=") {
			t.Fatalf("a chunked proxy upload was used despite a direct url: %s", call)
		}
	}

	rc, err := s.Get(context.Background(), "gen-9", "rootfs.delta.zst")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("direct download returned %d bytes, err %v", len(body), err)
	}
}

// TestPutProxyFallbackUploadsThroughTheControlPlane covers the object a presigned
// url cannot carry: the control plane answers "proxy" and the bytes still land.
func TestPutProxyFallbackUploadsThroughTheControlPlane(t *testing.T) {
	fake := &proxy{parts: map[string][][]byte{}, objects: map[string][]byte{}, direct: false}
	server := httptest.NewServer(fake)
	defer server.Close()
	fake.baseURL = server.URL

	s, err := DialFleet(server.URL, "fleet-token")
	if err != nil {
		t.Fatalf("DialFleet: %v", err)
	}
	s.ScratchDir = t.TempDir()

	payload := bytes.Repeat([]byte("proxied"), maxPart/7+1)
	if err := s.Put(context.Background(), "gen-8", "rootfs.ext4", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake.mu.Lock()
	stored := append([]byte(nil), fake.objects["gen-8/rootfs.ext4"]...)
	sawPart := false
	for _, call := range fake.seen {
		if strings.Contains(call, "part=") {
			sawPart = true
		}
	}
	fake.mu.Unlock()

	if !sawPart {
		t.Fatal("the proxy fallback did not use the chunked part protocol")
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("store holds %d bytes, want %d", len(stored), len(payload))
	}
}

// TestDirectPutRetriesWithTheWholeObject pins the retry contract: after a store
// failure the whole object is sent again, not its tail, so a partially sent
// attempt cannot leave a truncated object behind.
func TestDirectPutRetriesWithTheWholeObject(t *testing.T) {
	payload := bytes.Repeat([]byte("retry-me"), 4096)
	var bodies [][]byte
	attempts := 0
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.HasSuffix(call.url, "/url") {
			return okJSON(`{"ok":true,"data":{"mode":"direct","url":"https://store.test/bucket/gen-7/rootfs.delta","expiresInSeconds":900}}`), nil
		}
		attempts++
		bodies = append(bodies, append([]byte(nil), call.body...))
		if attempts == 1 {
			return statusJSON(http.StatusInternalServerError, "slow down"), nil
		}
		return statusJSON(http.StatusOK, ""), nil
	}
	transport.handlerServesURL = true
	s := newTestStore(t, transport)

	if err := s.Put(context.Background(), "gen-7", "rootfs.delta", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("store saw %d uploads, want 2 (a failure then a retry)", len(bodies))
	}
	for i, body := range bodies {
		if !bytes.Equal(body, payload) {
			t.Fatalf("upload %d sent %d bytes, want the whole %d-byte object", i, len(body), len(payload))
		}
	}
}

// TestDirectGetUsesTheFreshUrl proves a download re-signs rather than reusing a
// url that has since expired, which is what keeps a long resume from failing.
func TestDirectGetUsesTheFreshUrl(t *testing.T) {
	payload := []byte("generation-bytes")
	transport := &fakeTransport{}
	signs := 0
	gets := 0
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.HasSuffix(call.url, "/url") {
			signs++
			url := fmt.Sprintf("https://store.test/bucket/gen-6/rootfs.delta?sig=%d", signs)
			return okJSON(fmt.Sprintf(`{"ok":true,"data":{"mode":"direct","url":%q,"expiresInSeconds":900}}`, url)), nil
		}
		gets++
		if gets == 1 {
			return statusJSON(http.StatusForbidden, "Request has expired"), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"content-type": []string{"application/octet-stream"}},
			Body:       io.NopCloser(bytes.NewReader(payload)),
		}, nil
	}
	transport.handlerServesURL = true
	s := newTestStore(t, transport)

	rc, err := s.Get(context.Background(), "gen-6", "rootfs.delta")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("download returned %q, err %v", body, err)
	}
	if signs < 2 {
		t.Fatalf("only %d presign requests: an expired url must be re-signed", signs)
	}

	calls := transport.snapshot()
	var urls []string
	for _, call := range calls {
		if strings.Contains(call.url, "store.test") {
			urls = append(urls, call.url)
		}
	}
	if len(urls) < 2 || urls[len(urls)-1] == urls[0] {
		t.Fatalf("the retry reused the expired url: %v", urls)
	}
}

// TestDirectPutStagesAnUnseekableStream covers a compressed upload: the source
// cannot be rewound, so it is staged once and the retry still sends it whole.
func TestDirectPutStagesAnUnseekableStream(t *testing.T) {
	payload := bytes.Repeat([]byte("compressed"), 1024)
	var bodies [][]byte
	attempts := 0
	transport := &fakeTransport{}
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.HasSuffix(call.url, "/url") {
			return okJSON(`{"ok":true,"data":{"mode":"direct","url":"https://store.test/bucket/gen-5/rootfs.delta.zst","expiresInSeconds":900}}`), nil
		}
		attempts++
		bodies = append(bodies, append([]byte(nil), call.body...))
		if attempts == 1 {
			return statusJSON(http.StatusInternalServerError, "again"), nil
		}
		return statusJSON(http.StatusOK, ""), nil
	}
	transport.handlerServesURL = true
	s := newTestStore(t, transport)

	reader, writer := io.Pipe()
	go func() {
		_, err := writer.Write(payload)
		_ = writer.CloseWithError(err)
	}()
	if err := s.Put(context.Background(), "gen-5", "rootfs.delta.zst", reader, int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("store saw %d uploads, want 2", len(bodies))
	}
	for i, body := range bodies {
		if !bytes.Equal(body, payload) {
			t.Fatalf("upload %d sent %d bytes, want %d", i, len(body), len(payload))
		}
	}
}

// TestDirectPutRetriesAFileSourceRepositioned covers the source the real capture
// uses: a file. The HTTP transport closes the request body it is handed, so a
// body that is the file itself leaves the retry with a closed file, which then
// spins forever instead of resending. The body must stay unclosable.
func TestDirectPutRetriesAFileSourceRepositioned(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("delta-bytes"), 8192)
	path := filepath.Join(dir, "rootfs.delta.zst")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	attempts := 0
	var bodies [][]byte
	transport := &fakeTransport{}
	transport.handlerServesURL = true
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.HasSuffix(call.url, "/url") {
			return okJSON(`{"ok":true,"data":{"mode":"direct","url":"https://store.test/bucket/gen-4/rootfs.delta.zst","expiresInSeconds":900}}`), nil
		}
		attempts++
		bodies = append(bodies, append([]byte(nil), call.body...))
		if attempts == 1 {
			return statusJSON(http.StatusInternalServerError, "try again"), nil
		}
		return statusJSON(http.StatusOK, ""), nil
	}
	s := newTestStore(t, transport)

	if err := s.Put(context.Background(), "gen-4", "rootfs.delta.zst", file, int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("store saw %d uploads, want 2", attempts)
	}
	for i, body := range bodies {
		if !bytes.Equal(body, payload) {
			t.Fatalf("upload %d sent %d bytes, want the whole %d-byte object", i, len(body), len(payload))
		}
	}
}

// TestDirectPutGivesUpWhenTheStoreKeepsRejecting pins the failure path: a store
// that rejects every attempt must fail the upload instead of retrying until the
// caller's deadline expires, which would strand the dock in "archiving".
func TestDirectPutGivesUpWhenTheStoreKeepsRejecting(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 512)
	attempts := 0
	transport := &fakeTransport{}
	transport.handlerServesURL = true
	transport.handler = func(call recorded) (*http.Response, error) {
		if strings.HasSuffix(call.url, "/url") {
			return okJSON(`{"ok":true,"data":{"mode":"direct","url":"https://store.test/bucket/gen-3/rootfs.delta","expiresInSeconds":900}}`), nil
		}
		attempts++
		return statusJSON(http.StatusForbidden, "SignatureDoesNotMatch"), nil
	}
	s := newTestStore(t, transport)

	done := make(chan error, 1)
	go func() {
		done <- s.Put(context.Background(), "gen-3", "rootfs.delta", bytes.NewReader(payload), int64(len(payload)))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unreachable store must fail the upload")
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("upload retried forever (%d attempts) instead of giving up", attempts)
	}
	if attempts > 6 {
		t.Fatalf("gave up after %d attempts, want a small bounded number", attempts)
	}
}

func TestFleetStoreReadsTheBearerPerRequest(t *testing.T) {
	transport := &fakeTransport{}
	transport.handler = func(recorded) (*http.Response, error) {
		return statusJSON(http.StatusOK, `{"ok":true,"data":{"files":[]}}`), nil
	}
	s := newTestStore(t, transport)
	token := "hc_first"
	s.Bearer = func() string { return token }

	if _, err := s.List(context.Background(), "snap-1"); err != nil {
		t.Fatal(err)
	}
	token = "hc_rotated"
	if _, err := s.List(context.Background(), "snap-1"); err != nil {
		t.Fatal(err)
	}

	calls := transport.snapshot()
	if len(calls) != 2 || calls[0].header.Get("authorization") != "Bearer hc_first" || calls[1].header.Get("authorization") != "Bearer hc_rotated" {
		t.Fatalf("authorization headers = %+v", calls)
	}
}
