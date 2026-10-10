package store

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
	"strconv"
	"strings"
	"time"
)

const (
	// maxPart is the upload part size. The control plane sits behind a proxy
	// with a ~100MB request cap, so parts stay far below it.
	maxPart = 16 << 20
	// partTimeout bounds one metadata call or part upload; downloads stay
	// bounded by the caller, as with the S3 store.
	partTimeout = 10 * time.Minute
	// retryMin/retryMax bound the backoff on connection errors, 5xx, 401, 429.
	retryMin = time.Second
	retryMax = 30 * time.Second
	// maxBody caps how much of a response is buffered: object data streams.
	maxBody = 1 << 20
)

// doer performs one HTTP request; tests inject a recording fake.
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

// objectModeDirect means the control plane returned a presigned URL the host
// uses itself; anything else routes the bytes through the object proxy.
const objectModeDirect = "direct"

// objectModeProxy routes bytes through the control plane's chunked proxy, for
// objects a single presigned url cannot carry.
const objectModeProxy = "proxy"

// objectURL is the control plane's answer to a transfer request.
type objectURL struct {
	Mode             string `json:"mode"`
	URL              string `json:"url"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// FleetStore is the production Store on a fleet host: artifacts move through
// the control plane's chunked object proxy, so the bucket, its endpoint, and
// its credentials stay inside the control plane and never reach a host.
type FleetStore struct {
	baseURL string
	token   string
	client  doer
	// Bearer, when set, supplies the token per request so a credential issued
	// or rotated after dialing is picked up; nil keeps the dialed token.
	Bearer func() string
	// ScratchDir holds uploads whose length is unknown until read; empty uses
	// the process temp dir.
	ScratchDir string
	// Logf overrides the retry logger; tests capture output here.
	Logf func(format string, args ...any)
}

var _ Store = (*FleetStore)(nil)

// DialFleet points a store at the control plane's object proxy. Downloads and
// uploads carry the host's fleet bearer, the same one the agent uses.
func DialFleet(baseURL, token string) (*FleetStore, error) {
	if token == "" {
		return nil, errors.New("fleet store: token required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("fleet store: bad control plane url %q", baseURL)
	}
	return &FleetStore{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  http.DefaultClient,
	}, nil
}

// Put uploads one artifact. The control plane hands back a presigned URL so the
// bytes travel host-to-store directly instead of through the proxy, which is
// the difference between a few MB/s and the host's full uplink. The proxy
// remains the path for objects a single presigned PUT cannot carry.
func (s *FleetStore) Put(ctx context.Context, snapshotID, name string, r io.Reader, size int64) error {
	if size < 0 {
		// The length decides whether a direct PUT is possible at all, so an
		// unknown length spills to scratch first: the uploaded size is exact.
		spill, n, err := s.spill(r)
		if err != nil {
			return err
		}
		defer func() {
			_ = spill.Close()
			_ = os.Remove(spill.Name())
		}()
		return s.putKnown(ctx, snapshotID, name, spill, n)
	}
	return s.putKnown(ctx, snapshotID, name, r, size)
}

// putKnown uploads exactly size bytes, directly when the control plane allows it.
func (s *FleetStore) putKnown(ctx context.Context, snapshotID, name string, r io.Reader, size int64) error {
	u, err := s.objectURL(ctx, snapshotID, name, "put", size)
	if err != nil {
		return err
	}
	if u.Mode != objectModeDirect {
		return s.putParts(ctx, snapshotID, name, r, size)
	}
	// A retry must resend the same bytes, so the source has to be rewindable.
	// Streams that are not (a compression pipe, typically) are staged once.
	source, cleanup, err := s.rewindable(r, size)
	if err != nil {
		return err
	}
	defer cleanup()
	// The url is re-signed per attempt: a presigned url expires, and an upload
	// that outlives its TTL must not fail permanently.
	rejected := 0
	return s.attempt(ctx, "upload "+name, func(ctx context.Context) error {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			// A source that cannot be rewound is a bug, not a transient fault.
			return &apiError{status: http.StatusInternalServerError, message: err.Error(), fatal: true}
		}
		target := u.URL
		if fresh, err := s.objectURL(ctx, snapshotID, name, "put", size); err == nil && fresh.Mode == objectModeDirect && fresh.URL != "" {
			target = fresh.URL
		}
		err := s.putDirect(ctx, target, source, size)
		rejected++
		// A misconfigured store would otherwise be retried until the caller's
		// deadline, hiding the real cause.
		if rejected > directRetryLimit {
			return storeRejected(err, rejected)
		}
		return err
	})
}

// rewindable returns a seekable reader over exactly size bytes, staging the
// input to scratch when it cannot be rewound in place.
func (s *FleetStore) rewindable(r io.Reader, size int64) (io.ReadSeeker, func(), error) {
	if seeker, ok := r.(io.ReadSeeker); ok {
		return seeker, func() {}, nil
	}
	file, err := s.stage(io.LimitReader(r, size))
	if err != nil {
		return nil, func() {}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, func() {}, err
	}
	if info.Size() != size {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, func() {}, fmt.Errorf("upload %d bytes: reader ended after %d", size, info.Size())
	}
	return file, func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}, nil
}

// putDirect sends the object straight to the store and checks the status, since
// a rejected presigned PUT answers with the store's own error body.
func (s *FleetStore) putDirect(ctx context.Context, url string, r io.Reader, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, partTimeout)
	defer cancel()
	// The body must not be an io.Closer: the transport closes what it is given,
	// and the retry needs the same source again. Bounding it also guarantees
	// exactly size bytes go out.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("content-type", "application/octet-stream")
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	// Plain error, never apiError: a rejected signature has to be retried with a
	// fresh url, and apiError 4xx would be treated as permanent. The retry limit
	// in the caller turns a persistently bad store into a real failure.
	return fmt.Errorf("store refused the upload (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

// directRetryLimit bounds re-sign attempts before the store's own error is
// surfaced, so a misconfigured endpoint fails instead of retrying forever.
const directRetryLimit = 3

// storeRejected converts the final store error into a permanent failure, so a
// store that rejects every re-signed attempt fails the capture instead of
// retrying until the caller's deadline runs out.
func storeRejected(err error, attempts int) error {
	return &apiError{
		status:  http.StatusBadGateway,
		message: fmt.Sprintf("object store rejected %d attempts: %v", attempts, err),
		fatal:   true,
	}
}

// stage copies a reader to scratch so a stream can be uploaded more than once.
func (s *FleetStore) stage(r io.Reader) (*os.File, error) {
	dir := s.ScratchDir
	if dir == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, "docklet-staged-*")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(file, r); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

// objectURL asks the control plane for a direct transfer URL, or for the mode
// that says the object must go through the proxy instead.
func (s *FleetStore) objectURL(ctx context.Context, snapshotID, name, op string, size int64) (objectURL, error) {
	body, err := json.Marshal(struct {
		Op   string `json:"op"`
		Size int64  `json:"size"`
	}{Op: op, Size: size})
	if err != nil {
		return objectURL{}, err
	}
	var out objectURL
	route := objectRoute(snapshotID, name) + "/url"
	err = s.attempt(ctx, op+" url "+name, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, partTimeout)
		defer cancel()
		req, err := s.request(ctx, http.MethodPost, route, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		out = objectURL{}
		return s.jsonCall(req, &out)
	})
	// A control plane too old to know the route must not stall a capture: the
	// proxy it does offer still moves the bytes.
	if isMissingRoute(err) {
		s.logf("docklet: control plane has no transfer-url route, using the object proxy")
		return objectURL{Mode: objectModeProxy}, nil
	}
	if err != nil {
		return objectURL{}, err
	}
	if out.Mode == objectModeDirect && out.URL == "" {
		return objectURL{}, &apiError{status: http.StatusBadGateway, message: "control plane returned no transfer url"}
	}
	return out, nil
}

// Get streams one stored object, preferring a direct download. Absence reports a
// not-found error worded like MemStore's, so callers that treat any error as
// "generation gone" are unchanged.
func (s *FleetStore) Get(ctx context.Context, snapshotID, name string) (io.ReadCloser, error) {
	u, err := s.objectURL(ctx, snapshotID, name, "get", 0)
	if err != nil {
		return nil, err
	}
	if u.Mode != objectModeDirect {
		return s.getProxied(ctx, snapshotID, name)
	}
	return s.getDirect(ctx, u, snapshotID, name)
}

// getProxied streams one object through the control plane's object proxy.
func (s *FleetStore) getProxied(ctx context.Context, snapshotID, name string) (io.ReadCloser, error) {
	route := objectRoute(snapshotID, name)
	var body io.ReadCloser
	err := s.attempt(ctx, "download "+name, func(ctx context.Context) error {
		req, err := s.request(ctx, http.MethodGet, route, nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			body = resp.Body
			return nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		return failure(resp.StatusCode, raw, "no such object "+key(snapshotID, name))
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// getDirect downloads straight from the store, re-signing per attempt so an
// expired URL cannot strand the download.
func (s *FleetStore) getDirect(ctx context.Context, u objectURL, snapshotID, name string) (io.ReadCloser, error) {
	var body io.ReadCloser
	rejected := 0
	err := s.attempt(ctx, "download "+name, func(ctx context.Context) error {
		url := u.URL
		if attempt, err := s.objectURL(ctx, snapshotID, name, "get", 0); err == nil && attempt.Mode == objectModeDirect && attempt.URL != "" {
			url = attempt.URL
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			body = resp.Body
			return nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return &apiError{status: resp.StatusCode, message: "no such object " + key(snapshotID, name)}
		}
		// The store rejecting a signature is retryable: the next attempt signs
		// a fresh url. apiError would be classified as permanent and stop here.
		rejected++
		if rejected > directRetryLimit {
			return &apiError{status: resp.StatusCode, message: strings.TrimSpace(string(raw))}
		}
		return fmt.Errorf("store refused the download (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// spill copies an unknown-length reader to scratch so its exact length, and
// therefore its part count, is known before the first part is sent.
func (s *FleetStore) spill(r io.Reader) (*os.File, int64, error) {
	dir := s.ScratchDir
	if dir == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, "docklet-upload-*")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(file, r)
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, err
	}
	return file, n, nil
}

// putParts streams exactly size bytes as partCount(size) uploads, in order.
func (s *FleetStore) putParts(ctx context.Context, snapshotID, name string, r io.Reader, size int64) error {
	parts := partCount(size)
	buf := make([]byte, maxPart)
	for part := range parts {
		want := int64(maxPart)
		if remaining := size - int64(part)*maxPart; remaining < want {
			want = remaining
		}
		chunk := buf[:want]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return fmt.Errorf("put %s part %d of %d: %w", name, part, parts, err)
		}
		if err := s.putPart(ctx, snapshotID, name, part, parts, size, chunk); err != nil {
			return err
		}
	}
	// Truncating a longer reader silently would store a wrong object.
	var extra [1]byte
	if n, _ := io.ReadFull(r, extra[:]); n > 0 {
		return fmt.Errorf("put %s: reader holds more than the declared %d bytes", name, size)
	}
	return nil
}

// putPart sends one part; part parts-1 is the last and composes the object.
func (s *FleetStore) putPart(ctx context.Context, snapshotID, name string, part, parts int, size int64, body []byte) error {
	query := url.Values{
		"part":  {strconv.Itoa(part)},
		"parts": {strconv.Itoa(parts)},
		"size":  {strconv.FormatInt(size, 10)},
	}
	route := objectRoute(snapshotID, name) + "?" + query.Encode()
	return s.attempt(ctx, fmt.Sprintf("upload %s part %d/%d", name, part+1, parts), func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, partTimeout)
		defer cancel()
		req, err := s.request(ctx, http.MethodPut, route, bytes.NewReader(body))
		if err != nil {
			return err
		}
		// Raw bytes, never JSON: the proxy hands the body straight to the store.
		req.Header.Set("content-type", "application/octet-stream")
		return s.jsonCall(req, nil)
	})
}

// Delete removes every object of one generation.
func (s *FleetStore) Delete(ctx context.Context, snapshotID string) error {
	route := objectRoute(snapshotID, "")
	return s.attempt(ctx, "delete "+snapshotID, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, partTimeout)
		defer cancel()
		req, err := s.request(ctx, http.MethodDelete, route, nil)
		if err != nil {
			return err
		}
		return s.jsonCall(req, nil)
	})
}

// List returns the generation's stored objects exactly as the control plane
// reports them: paths are stored verbatim, so no suffix is interpreted here.
func (s *FleetStore) List(ctx context.Context, snapshotID string) ([]FileEntry, error) {
	route := objectRoute(snapshotID, "")
	var out struct {
		Files []FileEntry `json:"files"`
	}
	err := s.attempt(ctx, "list "+snapshotID, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, partTimeout)
		defer cancel()
		req, err := s.request(ctx, http.MethodGet, route, nil)
		if err != nil {
			return err
		}
		out.Files = nil
		return s.jsonCall(req, &out)
	})
	if err != nil {
		return nil, err
	}
	return out.Files, nil
}

// request builds one authenticated object-proxy request.
func (s *FleetStore) request(ctx context.Context, method, route string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+route, body)
	if err != nil {
		return nil, err
	}
	token := s.token
	if s.Bearer != nil {
		token = s.Bearer()
	}
	req.Header.Set("authorization", "Bearer "+token)
	return req, nil
}

// jsonCall performs one request whose body is a {ok,data} envelope, decoding
// data into out when out is non-nil.
func (s *FleetStore) jsonCall(req *http.Request, out any) error {
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failure(resp.StatusCode, raw, "")
	}
	var env struct {
		Ok      bool            `json:"ok"`
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("unreadable fleet object response: %w", err)
	}
	if !env.Ok {
		return &apiError{status: resp.StatusCode, code: env.Code, message: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// failure turns a non-2xx response into an error carrying the control plane's
// code and message. absent replaces it for a 404 when the caller wants the
// "generation is gone" wording.
func failure(status int, raw []byte, absent string) error {
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	if absent != "" && status == http.StatusNotFound {
		return &apiError{status: status, code: env.Code, message: absent}
	}
	if env.Message == "" {
		env.Message = strings.TrimSpace(string(raw))
	}
	return &apiError{status: status, code: env.Code, message: env.Message}
}

func (s *FleetStore) do(req *http.Request) (*http.Response, error) {
	if s.client == nil {
		return http.DefaultClient.Do(req)
	}
	return s.client.Do(req)
}

// attempt repeats op with bounded backoff until it succeeds, is permanently
// rejected, or ctx ends.
func (s *FleetStore) attempt(ctx context.Context, what string, op func(context.Context) error) error {
	delay := retryMin
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
		s.logf("docklet: fleet object %s failed: %s (retrying in %s)", what, describe(err), delay)
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = nextRetry(delay)
	}
}

func (s *FleetStore) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// apiError is a response that was not a successful envelope. fatal marks a
// failure that retrying cannot fix even though its status looks transient,
// such as a store that has already rejected every re-signed attempt.
type apiError struct {
	status  int
	code    string
	message string
	fatal   bool
}

func (e *apiError) Error() string {
	detail := e.message
	if e.code != "" {
		if detail == "" {
			detail = e.code
		} else {
			detail = e.code + ": " + detail
		}
	}
	if detail == "" {
		detail = http.StatusText(e.status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, detail)
}

// isMissingRoute reports the response of a control plane that does not serve the
// route at all, which is the one failure worth falling back on rather than
// reporting.
func isMissingRoute(err error) bool {
	var api *apiError
	if !errors.As(err, &api) {
		return false
	}
	return api.status == http.StatusNotFound || api.status == http.StatusMethodNotAllowed
}

// permanent reports whether retrying would fail identically: 4xx except 401
// (the token may be rotated) and 429 (back off and retry).
func (e *apiError) permanent() bool {
	if e.fatal {
		return true
	}
	return e.status >= 400 && e.status < 500 && e.status != http.StatusUnauthorized && e.status != http.StatusTooManyRequests
}

func permanent(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.permanent()
}

// describe renders a failed call for the operator, calling out the token case
// so a 401 is never mistaken for a transient outage.
func describe(err error) string {
	var api *apiError
	if errors.As(err, &api) && api.status == http.StatusUnauthorized {
		return "unauthorized, check DOCKLET_FLEET_TOKEN (" + err.Error() + ")"
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

// nextRetry doubles d up to the ceiling.
func nextRetry(d time.Duration) time.Duration {
	if d *= 2; d > retryMax {
		return retryMax
	}
	return d
}

// objectRoute is the proxy path for one generation object; an empty name
// addresses the generation itself.
func objectRoute(snapshotID, name string) string {
	route := "/v1/fleet/objects/" + url.PathEscape(snapshotID)
	if name != "" {
		route += "/" + escapeSegments(name)
	}
	return route
}

// escapeSegments escapes each segment of a generation-relative path but keeps
// its separators.
func escapeSegments(p string) string {
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// partCount is the number of at-most-maxPart uploads needed for size bytes. An
// empty artifact still needs one part to exist at all.
func partCount(size int64) int {
	if size <= 0 {
		return 1
	}
	return int((size + maxPart - 1) / maxPart)
}
