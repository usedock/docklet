package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/usedock/docklet/internal/local"
)

// Tuning for push and pull; tests shorten the waits.
const (
	partAttempts     = 5
	partBusyAttempts = 20 // part_upload_busy is the control plane queueing parts, not a fault
	completeAttempts = 6
	downloadAttempts = 6
	defaultBackoff   = time.Second
	defaultPoll      = 2 * time.Second
	maxBackoff       = 30 * time.Second
	// defaultIdle is how long a transfer connection may move no bytes before the attempt fails.
	defaultIdle = time.Minute
	// defaultHeaderWait is how long a finished request waits for response headers. The control
	// plane may queue a part for up to 60s before it answers 429 part_upload_busy, so it must
	// outlast that.
	defaultHeaderWait = 90 * time.Second
	pushReadyTimeout  = 10 * time.Minute
	exportTimeout     = 30 * time.Minute
)

// contractMessages are readable texts for the error codes of the push and pull contract.
var contractMessages = map[string]string{
	"import_too_large":         "the disk is larger than this control plane accepts for an import",
	"import_checksum_mismatch": "the control plane received different bytes than were sent (checksum mismatch); try again",
	"runtime_unsupported":      "the control plane has no microVM host to run an imported dock; push needs a microVM host",
	"export_base_unsupported":  "this dock cannot be pulled: its base is not a Dockyard image (only docks built from a Dockyard image can move)",
	"export_failed":            "the host could not export the dock; try again",
	"export_interrupted":       "the export was interrupted on the control plane; try again",
	"zdr_blocks_exports":       "exports are disabled in this organization while zero data retention is on",
	"import_limit_reached":     "too many imports are open in this organization; wait for one to finish or expire",
	"export_limit_reached":     "too many exports are live in this organization; they expire after 24 hours",
}

// retryableComplete are failures of complete that a later call can fix without re-uploading:
// the control plane keeps the assembled upload and complete is idempotent.
var retryableComplete = map[string]bool{
	"request_failed": true, "internal_error": true, "bad_gateway": true, "service_unavailable": true,
	"no_host_capacity": true, "capacity_exhausted": true, "base_image_unavailable": true, "image_unavailable": true,
}

// contractError replaces the message of a known contract code with a readable one.
func contractError(err error) error {
	var ae *Error
	if errors.As(err, &ae) {
		if m, ok := contractMessages[ae.Code]; ok {
			return newError(ae.Code, m)
		}
	}
	return err
}

func (a *App) backoff() time.Duration {
	if a.xferBackoff > 0 {
		return a.xferBackoff
	}
	return defaultBackoff
}

// backoffFor is the wait before retry number n (1-based): exponential, capped, and jittered
// so parallel clients do not retry in step.
func (a *App) backoffFor(n int) time.Duration {
	d := a.backoff() << min(max(n-1, 0), 10)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return jitter(d)
}

// jitter returns a duration in [d/2, d].
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1)
}

func (a *App) idleWait() time.Duration {
	if a.xferIdle > 0 {
		return a.xferIdle
	}
	return defaultIdle
}

func (a *App) headerWait() time.Duration {
	if a.xferHeaderWait > 0 {
		return a.xferHeaderWait
	}
	return defaultHeaderWait
}

func (a *App) pollEvery() time.Duration {
	if a.xferPoll > 0 {
		return a.xferPoll
	}
	return defaultPoll
}

func (a *App) transferHTTP() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	// No overall timeout: parts are large; transferDo fails a connection that goes idle instead.
	return &http.Client{}
}

// errStalled marks an attempt that moved no bytes for too long; it is retried like any
// transport failure.
var errStalled = errors.New("the connection stalled")

// watchdog cancels a request when no byte has moved for its current deadline.
type watchdog struct {
	t      *time.Timer
	cancel context.CancelCauseFunc
}

func (w *watchdog) kick(d time.Duration) { w.t.Reset(d) }

// watchBody feeds the watchdog while the request body is written, then waits for headers.
type watchBody struct {
	io.ReadCloser
	w          *watchdog
	idle, wait time.Duration
}

func (b *watchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.w.kick(b.wait)
	} else {
		b.w.kick(b.idle)
	}
	return n, err
}

// watchResp feeds the watchdog as the response body is read and releases the request on Close.
type watchResp struct {
	io.ReadCloser
	w    *watchdog
	ctx  context.Context
	idle time.Duration
}

func (r *watchResp) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && err != io.EOF && context.Cause(r.ctx) == errStalled {
		err = errStalled
	}
	if err == nil {
		r.w.kick(r.idle)
	}
	return n, err
}

func (r *watchResp) Close() error {
	r.w.t.Stop()
	err := r.ReadCloser.Close()
	r.w.cancel(nil)
	return err
}

// transferDo sends req with an idle watchdog instead of a total timeout, so a stalled
// connection fails and is retried while a long transfer that keeps moving is never cut. A
// finished request waits up to headerWait for headers (the control plane may queue a part).
func (a *App) transferDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	idle, wait := a.idleWait(), a.headerWait()
	wctx, cancel := context.WithCancelCause(ctx)
	w := &watchdog{cancel: cancel}
	first := wait
	if req.Body != nil {
		first = idle
	}
	w.t = time.AfterFunc(first, func() { cancel(errStalled) })
	req = req.WithContext(wctx)
	if req.Body != nil {
		req.Body = &watchBody{ReadCloser: req.Body, w: w, idle: idle, wait: wait}
	}
	resp, err := a.transferHTTP().Do(req)
	if err != nil {
		w.t.Stop()
		stalled := context.Cause(wctx) == errStalled
		cancel(nil)
		if stalled {
			return nil, errStalled
		}
		return nil, err
	}
	w.kick(idle)
	resp.Body = &watchResp{ReadCloser: resp.Body, w: w, ctx: wctx, idle: idle}
	return resp, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// progress prints throttled progress lines to stderr unless quiet.
type progress struct {
	w     io.Writer
	label string
	total int64
	last  time.Time
}

func (a *App) newProgress(quiet bool, label string, total int64) *progress {
	if quiet {
		return &progress{}
	}
	return &progress{w: a.Stderr, label: label, total: total}
}

func (p *progress) update(done int64, force bool) {
	if p.w == nil || (!force && time.Since(p.last) < 2*time.Second) {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(p.w, "%s %s / %s\n", p.label, local.HumanBytes(done), local.HumanBytes(p.total))
}

func (a *App) tempDir(eng *local.Engine) (string, func(), error) {
	// A crash or SIGKILL skips the deferred cleanup; sweep what an earlier run left behind.
	eng.CleanStaleTemp()
	d, err := eng.TempDir()
	if err != nil {
		return "", nil, localError(err)
	}
	return d, func() { _ = os.RemoveAll(d) }, nil
}

// ---- push ----

type importPart struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type importPlan struct {
	ImportID string `json:"importId"`
	Upload   struct {
		Mode     string       `json:"mode"`
		PartSize int64        `json:"partSize"`
		Parts    []importPart `json:"parts"`
	} `json:"upload"`
}

type donePart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
}

func (a *App) cmdPush(ctx context.Context, args []string) error {
	c := a.flags("push", "push <dock> --to <context> [--name N] [--workspace ID] [--stop] [--rm] [--image X --disk 10gb (gVisor docks)]")
	image := c.fs.String("image", "", "gVisor docks: Dockyard image the remote dock starts from (default "+local.DefaultImage+")")
	disk := c.fs.String("disk", "", "gVisor docks: disk of the remote dock, e.g. 20gb (default: the dock's, else "+strconv.Itoa(local.DefaultDiskGiB)+" GiB)")
	to := c.fs.String("to", "", "context to push to (a control plane, not local)")
	name := c.fs.String("name", "", "name of the new remote dock (default: the local name)")
	ws := c.fs.String("workspace", "", "workspace id (default: the context's, else your only workspace)")
	stop := c.fs.Bool("stop", false, "stop the dock first when it is running")
	rm := c.fs.Bool("rm", false, "delete the local dock after a successful push")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if *to == "" {
		return usagef("--to <context> is required")
	}
	cfgPath, err := a.configPath()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	cl, defWS, err := a.remote(cfg, *to)
	if err != nil {
		return err
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	rec, err := eng.Lookup(pos[0])
	if err != nil {
		return localError(err)
	}
	var opt local.PushOpts
	gv := rec.Runtime == local.RuntimeGVisor
	switch {
	case gv:
		opt.Image = *image
		if opt.Image == "" {
			opt.Image = local.DefaultImage
		}
		if c.isSet("disk") {
			if opt.DiskGiB, err = ParseDiskGiB(*disk); err != nil {
				return usagef("--disk: %v", err)
			}
		}
		// A gVisor dock has no disk image, so the remote dock starts from a Dockyard image;
		// fail on an unknown one before anything is stopped or built.
		if _, err := eng.ResolveImage(ctx, opt.Image); err != nil {
			return localError(err)
		}
		fmt.Fprintf(a.Stderr, "note: only /home/user/work is pushed; packages and files outside it in this gVisor dock are not. The remote dock starts from %s with your work dir.\n", opt.Image)
	case c.isSet("image") || c.isSet("disk"):
		return usagef("--image and --disk apply to gVisor docks only; this dock's own disk is pushed")
	}
	tgt := &target{doer: cl, Workspace: defWS}
	wsID, err := a.resolveWorkspace(ctx, tgt, *ws, *c.json)
	if err != nil {
		return err
	}
	dir, cleanup, err := a.tempDir(eng)
	if err != nil {
		return err
	}
	defer cleanup()
	// Sized from the data the disk holds, not its virtual size: the flattened raw is sparse.
	if err := eng.CheckPushSpace(rec); err != nil {
		return localError(err)
	}

	flat, err := eng.FlattenWith(ctx, pos[0], dir, *stop, opt)
	if err != nil {
		return localError(err)
	}
	zst := filepath.Join(dir, "rootfs.ext4.zst")
	if !*c.json {
		fmt.Fprintln(a.Stderr, "compressing the disk")
	}
	sum, size, err := local.CompressFile(ctx, flat.RawPath, zst)
	_ = os.Remove(flat.RawPath)
	if err != nil {
		return localError(err)
	}

	remoteName := *name
	if remoteName == "" {
		remoteName = rec.Name
	}
	body := map[string]any{"workspaceId": wsID, "image": flat.Image, "cpus": rec.CPUs, "memoryMiB": rec.MemoryMiB,
		"diskGiB": flat.DiskGiB, "sizeBytes": size, "sha256": sum}
	if remoteName != "" {
		body["name"] = remoteName
	}
	raw, err := cl.Do(ctx, "POST", "/docks/import", nil, body)
	if err != nil {
		return contractError(err)
	}
	var plan importPlan
	if err := json.Unmarshal(raw, &plan); err != nil || plan.ImportID == "" || plan.Upload.PartSize <= 0 {
		return newError("bad_response", "unexpected import plan")
	}
	parts, err := a.uploadParts(ctx, cl, plan, zst, size, *c.json)
	if err != nil {
		return err
	}
	raw, err = a.completeImport(ctx, cl, plan.ImportID, parts)
	if err != nil {
		return err
	}
	var d dock
	if err := json.Unmarshal(raw, &d); err != nil || d.ID == "" {
		return newError("bad_response", "unexpected response to complete")
	}
	raw, err = a.waitRemoteRunning(ctx, cl, d.ID, *c.json)
	if err != nil {
		return err
	}
	if *rm {
		if err := eng.Remove(ctx, rec.ID); err != nil {
			return fmt.Errorf("pushed, but the local dock was not removed: %w", localError(err))
		}
	}
	if *c.json {
		return a.printJSON(raw)
	}
	_ = json.Unmarshal(raw, &d)
	fmt.Fprintf(a.Stdout, "pushed %s to %s as %s (%s) state=%s\n", pos[0], *to, d.label(), d.ID, d.State)
	return nil
}

func (a *App) uploadParts(ctx context.Context, cl *Client, plan importPlan, zst string, size int64, quiet bool) ([]donePart, error) {
	n := int((size + plan.Upload.PartSize - 1) / plan.Upload.PartSize)
	if plan.Upload.Mode != "direct" && plan.Upload.Mode != "proxy" {
		return nil, newError("bad_response", fmt.Sprintf("unknown upload mode %q", plan.Upload.Mode))
	}
	direct := plan.Upload.Mode == "direct"
	urls := map[int]string{}
	for _, p := range plan.Upload.Parts {
		urls[p.Number] = p.URL
	}
	f, err := os.Open(zst)
	if err != nil {
		return nil, newError("transfer_failed", err.Error())
	}
	defer f.Close()
	pr := a.newProgress(quiet, "uploaded", size)
	out := make([]donePart, 0, n)
	for i := 1; i <= n; i++ {
		off := int64(i-1) * plan.Upload.PartSize
		length := min(plan.Upload.PartSize, size-off)
		target := urls[i]
		if direct && target == "" {
			return nil, newError("bad_response", fmt.Sprintf("the import plan has no url for part %d", i))
		}
		if !direct {
			target = strings.TrimRight(cl.BaseURL, "/") + "/v1/docks/import/" + url.PathEscape(plan.ImportID) + "/parts/" + strconv.Itoa(i)
		}
		var etag string
		fails, busy := 0, 0
		for {
			etag, err = a.putPart(ctx, cl, target, direct, io.NewSectionReader(f, off, length), length)
			if err == nil {
				break
			}
			var perm *permanentError
			if errors.As(err, &perm) || ctx.Err() != nil {
				return nil, newError("upload_failed", fmt.Sprintf("part %d of %d: %v", i, n, err))
			}
			// A busy answer has its own, larger budget: the control plane is only queueing.
			count, limit := &fails, partAttempts
			var be *busyError
			if errors.As(err, &be) {
				count, limit = &busy, partBusyAttempts
			}
			*count++
			if *count >= limit {
				return nil, newError("upload_failed", fmt.Sprintf("part %d of %d: %v", i, n, err))
			}
			if err := sleepCtx(ctx, a.backoffFor(*count)); err != nil {
				return nil, err
			}
		}
		out = append(out, donePart{Number: i, ETag: etag})
		pr.update(off+length, i == n)
	}
	return out, nil
}

// completeImport finishes the upload, retrying transient failures; the parts are not sent again.
func (a *App) completeImport(ctx context.Context, cl *Client, importID string, parts []donePart) (json.RawMessage, error) {
	for attempt := 1; ; attempt++ {
		raw, err := cl.Do(ctx, "POST", "/docks/import/"+url.PathEscape(importID)+"/complete", nil, map[string]any{"parts": parts})
		if err == nil {
			return raw, nil
		}
		var ae *Error
		if !errors.As(err, &ae) || !(retryableComplete[ae.Code] || strings.HasPrefix(ae.Code, "http_5")) ||
			ctx.Err() != nil || attempt >= completeAttempts {
			return nil, contractError(err)
		}
		if err := sleepCtx(ctx, a.backoffFor(attempt)); err != nil {
			return nil, err
		}
	}
}

// gibBytes is a disk size in GiB as bytes, clamped so a bad record cannot overflow.
func gibBytes(gib int) int64 {
	return int64(min(max(gib, 0), 1<<30)) << 30
}

// busyError is the control plane asking the client to wait (429 part_upload_busy).
type busyError struct{ msg string }

func (e *busyError) Error() string { return e.msg }

// permanentError is a failure a retry cannot fix.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

// putPart uploads one part. A direct PUT goes to a presigned URL and carries no credentials;
// a proxy PUT goes to the control plane with the API key.
func (a *App) putPart(ctx context.Context, cl *Client, target string, direct bool, body io.Reader, length int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		return "", &permanentError{err.Error()}
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	if !direct {
		req.Header.Set("Authorization", "Bearer "+cl.Key)
		req.Header.Set("Accept", "application/json")
	}
	resp, err := a.transferDo(ctx, req)
	if err != nil {
		return "", errors.New(redact(stripURL(err).Error(), cl.Key))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := resp.Status
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Message != "" {
			msg += ": " + env.Message
			if m, ok := contractMessages[env.Code]; ok {
				msg = m
			}
		}
		if env.Code == "part_upload_busy" {
			return "", &busyError{msg}
		}
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
			return "", errors.New(msg)
		}
		return "", &permanentError{msg}
	}
	etag := resp.Header.Get("ETag")
	if etag == "" && !direct {
		var env struct {
			Data struct {
				ETag string `json:"etag"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		etag = env.Data.ETag
	}
	if etag == "" {
		return "", &permanentError{"the upload answered without an ETag"}
	}
	return etag, nil
}

// stripURL drops the URL from a transport error: a presigned URL is a credential for the
// object and must not reach the terminal, logs or --json output.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Host
		}
		return fmt.Errorf("%s %s: %w", ue.Op, host, ue.Err)
	}
	return err
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// waitRemoteRunning polls the new dock until it is running and returns its JSON.
func (a *App) waitRemoteRunning(ctx context.Context, cl *Client, id string, quiet bool) (json.RawMessage, error) {
	if !quiet {
		fmt.Fprintln(a.Stderr, "waiting for the remote dock to start")
	}
	deadline := time.Now().Add(pushReadyTimeout)
	for {
		raw, err := cl.Do(ctx, "GET", "/docks/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return nil, err
		}
		var d dock
		_ = json.Unmarshal(raw, &d)
		switch d.State {
		case "ready", "idle", "running":
			return raw, nil
		case "error", "failed", "destroyed", "deleted":
			return nil, newError("push_failed", fmt.Sprintf("the remote dock %s ended in state %q", id, d.State))
		}
		if time.Now().After(deadline) {
			return nil, newError("push_timeout", fmt.Sprintf("the remote dock %s is still %q after %s", id, d.State, pushReadyTimeout))
		}
		if err := sleepCtx(ctx, a.pollEvery()); err != nil {
			return nil, err
		}
	}
}

// ---- pull ----

type exportInfo struct {
	State     string `json:"state"`
	Error     string `json:"error"`
	Image     string `json:"image"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memoryMiB"`
	DiskGiB   int    `json:"diskGiB"`
	SizeBytes int64  `json:"sizeBytes"`
	// DataBytes is the data size recorded at export; 0 for an export from before it was recorded.
	DataBytes int64  `json:"dataBytes"`
	SHA256    string `json:"sha256"`
	BaseImage string `json:"baseImage"`
	URL       string `json:"url"`
	// Format is "disk" (the default when absent) or "work-tar", the work directory of a gVisor dock.
	Format string `json:"format"`
}

const formatWorkTar = "work-tar"

// gvExportUnsupported is the error for a control plane or host that cannot export gVisor docks.
func gvExportUnsupported() error {
	return newError("gvisor_export_unsupported", "this control plane (or the dock's host) cannot export gVisor docks yet; update the control plane and the dock's host, then pull again")
}

// gvExportRefused maps a refusal of a gVisor export (an older control plane, or a host without
// work-export) to gvExportUnsupported and passes other errors through.
func gvExportRefused(err error) error {
	var ae *Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case "runtime_unsupported", "not_supported", "export_base_unsupported":
			return gvExportUnsupported()
		}
	}
	return err
}

// withSource adds the source of the new dock ("disk" or "work") to its JSON.
func withSource(v any, source string) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["source"] = source
	return m, nil
}

// pulled reports a finished pull; --json adds where the new dock's files came from.
func (a *App) pulled(asJSON bool, d local.Dock, source, remote, from string) error {
	if asJSON {
		m, err := withSource(d, source)
		if err != nil {
			return err
		}
		return a.printValue(m)
	}
	fmt.Fprintf(a.Stdout, "pulled %s from %s as %s (%s) state=%s\n", remote, from, dock(d).label(), d.ID, d.State)
	return nil
}

func (a *App) cmdPull(ctx context.Context, args []string) error {
	c := a.flags("pull", "pull <dock> --from <context> [--name N] [--runtime qemu|gvisor]")
	from := c.fs.String("from", "", "context to pull from (a control plane, not local)")
	name := c.fs.String("name", "", "name of the new local dock (default: the remote name)")
	runtimeFlag := c.fs.String("runtime", "", "runtime of the new local dock: qemu (default, the whole disk comes along) or gvisor (Linux with Docker and runsc, no KVM needed; only /home/user/work is copied). Default from DOCK_LOCAL_RUNTIME")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if *from == "" {
		return usagef("--from <context> is required")
	}
	cfgPath, err := a.configPath()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	cl, defWS, err := a.remote(cfg, *from)
	if err != nil {
		return err
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	tgt := &target{doer: cl, Workspace: defWS}
	id, err := a.resolveDock(ctx, tgt, pos[0])
	if err != nil {
		return err
	}
	raw, err := cl.Do(ctx, "GET", "/docks/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	var rd dock
	_ = json.Unmarshal(raw, &rd)
	localName := *name
	if localName == "" {
		localName = rd.Name
	}
	gvRemote := rd.Runtime == local.RuntimeGVisor
	// The runtime's preflight runs before the export, so a host that cannot run it fails early.
	rt, err := eng.PullRuntime(ctx, *runtimeFlag, a.env("DOCK_LOCAL_RUNTIME"))
	if err != nil {
		return localError(err)
	}
	// Fail on a taken name before the long export and download.
	if err := eng.CheckName(localName); err != nil {
		return localError(err)
	}
	if gvRemote && rt == local.RuntimeGVisor {
		image := rd.Image
		if image == "" {
			image = "image"
		}
		fmt.Fprintf(a.Stderr, "note: the remote dock runs on gVisor; the new dock starts from a fresh %s with only /home/user/work copied.\n", image)
	} else if !gvRemote && rt == local.RuntimeGVisor {
		fmt.Fprintln(a.Stderr, "note: only /home/user/work is copied into the gVisor dock; packages and files outside it on the remote dock are not.")
	}

	body := map[string]any{}
	if gvRemote {
		body["formats"] = []string{formatWorkTar}
	}
	raw, err = cl.Do(ctx, "POST", "/docks/"+url.PathEscape(id)+"/export", nil, body)
	if err != nil {
		if gvRemote {
			err = gvExportRefused(err)
		}
		return contractError(err)
	}
	var started struct {
		ExportID string `json:"exportId"`
		Format   string `json:"format"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.ExportID == "" {
		return newError("bad_response", "unexpected export response")
	}
	if !*c.json {
		fmt.Fprintln(a.Stderr, "exporting the dock")
	}
	info, err := a.waitExport(ctx, cl, started.ExportID)
	if err != nil {
		if gvRemote {
			err = gvExportRefused(err)
		}
		return err
	}
	if info.Format == "" {
		info.Format = started.Format
	}
	workTar := info.Format == formatWorkTar
	if gvRemote != workTar {
		if gvRemote {
			// An older control plane that ignored the request and sent a disk.
			return gvExportUnsupported()
		}
		return newError("bad_response", "the export is a work archive, but the remote dock is not a gVisor dock")
	}
	source := "disk"
	if workTar {
		source = "work"
	}
	gv := rt == local.RuntimeGVisor
	var cached local.CachedImage
	// A qemu dock made from a work tar starts from the base the export names, else the remote
	// dock's own image, else the default; the note names the image that is used.
	baseImage := cmp.Or(info.BaseImage, rd.Image, local.DefaultImage)
	if workTar && !gv {
		// The part file, the tar, its canonical copy and the guest's disk grow together; a base
		// image that is not cached yet is downloaded first.
		if err := eng.CheckFreeSpace(eng.PullSpaceNeed(ctx, info.SizeBytes, 3*info.DataBytes, 0, baseImage), "pull"); err != nil {
			return localError(err)
		}
		fmt.Fprintf(a.Stderr, "note: the remote dock runs on gVisor; the new dock starts from a fresh %s with only /home/user/work copied.\n", baseImage)
		if !*c.json {
			fmt.Fprintf(a.Stderr, "preparing the %s image\n", baseImage)
		}
		if _, err = eng.EnsureImage(ctx, baseImage); err != nil {
			return localError(err)
		}
	} else if workTar {
		// The part file, the tar it expands to, and the unpacked files in Docker's data directory.
		if err := eng.CheckFreeSpace(eng.PullSpaceNeedGVisor(info.SizeBytes, info.DataBytes, 0), "pull"); err != nil {
			return localError(err)
		}
	} else if gv {
		// No base image and no qcow2: the part file beside the sparse raw disk, then the raw
		// disk alone while its work directory is copied out (into Docker's data directory).
		if err := eng.CheckFreeSpace(eng.PullSpaceNeedGVisor(info.SizeBytes, info.DataBytes, gibBytes(info.DiskGiB)), "pull"); err != nil {
			return localError(err)
		}
	} else {
		if info.BaseImage == "" {
			return contractError(newError("export_base_unsupported", ""))
		}
		// Peak: the part file beside the sparse raw disk, then that raw disk beside the new qcow2,
		// on top of a base image that may still have to be downloaded.
		if err := eng.CheckFreeSpace(eng.PullSpaceNeed(ctx, info.SizeBytes, info.DataBytes, gibBytes(info.DiskGiB), info.BaseImage), "pull"); err != nil {
			return localError(err)
		}
		if !*c.json {
			fmt.Fprintf(a.Stderr, "preparing the %s image\n", info.BaseImage)
		}
		cached, err = eng.EnsureImage(ctx, info.BaseImage)
		if err != nil {
			return localError(err)
		}
	}
	dir, cleanup, err := a.tempDir(eng)
	if err != nil {
		return err
	}
	defer cleanup()
	part := filepath.Join(dir, "export.zst")
	if err := a.download(ctx, cl, started.ExportID, info, part, *c.json); err != nil {
		return err
	}
	if workTar {
		// The tar is about the recorded data size (plus headers); without one, the disk size bounds it.
		limit := int64(info.DiskGiB) << 30
		if limit <= 0 {
			limit = local.DefaultMaxRawBytes
		}
		if info.DataBytes > 0 {
			limit = min(limit, info.DataBytes+info.DataBytes/8+64<<20)
		}
		tarPath := filepath.Join(dir, "work.tar")
		if err := local.DecompressWorkFile(ctx, part, tarPath, limit); err != nil {
			return localError(err)
		}
		_ = os.Remove(part)
		if !gv {
			d, err := eng.ImportWorkTarQEMU(ctx, local.WorkTarQEMU{Name: localName, Image: baseImage, TarPath: tarPath, CPUs: info.CPUs,
				MemoryMiB: info.MemoryMiB, DiskGiB: max(info.DiskGiB, local.DefaultDiskGiB), MaxBytes: limit})
			if err != nil {
				return localError(err)
			}
			return a.pulled(*c.json, d, source, pos[0], *from)
		}
		d, err := eng.ImportWorkTar(ctx, local.ImportWorkTar{Name: localName, TarPath: tarPath, CPUs: info.CPUs, MemoryMiB: info.MemoryMiB, MaxBytes: limit})
		if err != nil {
			return localError(err)
		}
		return a.pulled(*c.json, d, source, pos[0], *from)
	}
	max := int64(info.DiskGiB) << 30
	if max <= 0 {
		max = local.DefaultMaxRawBytes
	}
	rawDisk := filepath.Join(dir, "disk.raw")
	if err := local.DecompressFile(ctx, part, rawDisk, max); err != nil {
		return localError(err)
	}
	_ = os.Remove(part)
	if gv {
		d, err := eng.ImportWork(ctx, local.ImportWork{Name: localName, RawPath: rawDisk, CPUs: info.CPUs, MemoryMiB: info.MemoryMiB})
		if err != nil {
			return localError(err)
		}
		return a.pulled(*c.json, d, source, pos[0], *from)
	}
	d, err := eng.Import(ctx, local.Import{Name: localName, Image: info.BaseImage, Cached: cached, RawPath: rawDisk,
		CPUs: info.CPUs, MemoryMiB: info.MemoryMiB, DiskGiB: info.DiskGiB})
	if err != nil {
		return localError(err)
	}
	return a.pulled(*c.json, d, source, pos[0], *from)
}

func (a *App) waitExport(ctx context.Context, cl *Client, exportID string) (exportInfo, error) {
	deadline := time.Now().Add(exportTimeout)
	for {
		info, err := a.getExport(ctx, cl, exportID)
		if err != nil {
			return info, err
		}
		switch info.State {
		case "ready":
			if info.URL == "" || info.SizeBytes <= 0 || info.SHA256 == "" {
				return info, newError("bad_response", "the export is ready but has no url, size or sha256")
			}
			return info, nil
		case "failed":
			code := info.Error
			if _, ok := contractMessages[code]; ok {
				return info, contractError(newError(code, ""))
			}
			if code == "" {
				code = "the host gave no reason"
			}
			return info, newError("export_failed", code)
		}
		if time.Now().After(deadline) {
			return info, newError("export_timeout", fmt.Sprintf("the export is still %q after %s", info.State, exportTimeout))
		}
		if err := sleepCtx(ctx, a.pollEvery()); err != nil {
			return info, err
		}
	}
}

func (a *App) getExport(ctx context.Context, cl *Client, exportID string) (exportInfo, error) {
	raw, err := cl.Do(ctx, "GET", "/docks/exports/"+url.PathEscape(exportID), nil, nil)
	if err != nil {
		return exportInfo{}, contractError(err)
	}
	var info exportInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, newError("bad_response", "unexpected export state")
	}
	return info, nil
}

// download fetches the presigned url into part, resuming with Range after an interruption
// (a fresh url is requested between attempts, since each lasts 15 minutes), then verifies
// the size and sha256.
func (a *App) download(ctx context.Context, cl *Client, exportID string, info exportInfo, part string, quiet bool) error {
	pr := a.newProgress(quiet, "downloaded", info.SizeBytes)
	var dl dlState
	// fails counts attempts in a row that moved nothing; an attempt that takes the file past
	// its largest size so far resets it, so a long pull on a flaky link keeps going while it
	// makes progress, and regrowing bytes a restart discarded never refills the budget.
	fails := 0
	best := fileSize(part)
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, a.backoffFor(max(fails, 1))); err != nil {
				return err
			}
			fresh, err := a.getExport(ctx, cl, exportID)
			if err != nil {
				return err
			}
			if fresh.State != "ready" || fresh.URL == "" {
				return newError("download_failed", "the export is no longer available; run the pull again")
			}
			info.URL = fresh.URL
		}
		done, err := a.fetchRange(ctx, cl, info, part, pr, &dl)
		if err == nil && done {
			break
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			return newError("download_failed", perm.msg)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if sz := fileSize(part); sz > best {
			best = sz
			fails = 0
			continue
		}
		fails++
		if fails >= downloadAttempts {
			if err == nil {
				err = errors.New("the download ended early")
			}
			return newError("download_failed", err.Error())
		}
	}
	st, err := os.Stat(part)
	if err != nil || st.Size() != info.SizeBytes {
		return newError("download_failed", fmt.Sprintf("downloaded size does not match the export (%d bytes expected)", info.SizeBytes))
	}
	sum, err := local.FileSHA256(ctx, part)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return newError("download_failed", err.Error())
	}
	if !strings.EqualFold(sum, info.SHA256) {
		_ = os.Remove(part)
		return newError("checksum_mismatch", "the downloaded disk does not match the export's sha256; it was discarded, run the pull again")
	}
	return nil
}

// fetchRange makes one request that continues part from its current size. done reports
// that part now holds the whole export.
func (a *App) fetchRange(ctx context.Context, cl *Client, info exportInfo, part string, pr *progress, st *dlState) (bool, error) {
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if have > info.SizeBytes {
		if err := os.Truncate(part, 0); err != nil {
			return false, &permanentError{err.Error()}
		}
		have = 0
	}
	if have == info.SizeBytes {
		return true, nil
	}
	req, err := exportRequest(ctx, cl, info.URL)
	if err != nil {
		return false, &permanentError{err.Error()}
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		// If the object changed since the first response the server answers 200 with the whole
		// new body instead of a range of it.
		if st.validator != "" {
			req.Header.Set("If-Range", st.validator)
		}
	}
	resp, err := a.transferDo(ctx, req)
	if err != nil {
		return false, stripURL(err)
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// Appending a range that is not the one asked for would corrupt the file: start over.
		if why := badContentRange(resp.Header, have, info.SizeBytes); why != "" {
			return false, restartDownload(part, st, why)
		}
		if v := validatorOf(resp.Header); st.validator != "" && v != "" && v != st.validator {
			return false, restartDownload(part, st, "the export changed on the server")
		}
		if st.validator == "" {
			st.validator = validatorOf(resp.Header)
		}
		flags |= os.O_APPEND
	case http.StatusOK:
		// The server ignored Range, or If-Range said the object changed: start over.
		flags |= os.O_TRUNC
		have = 0
		st.validator = validatorOf(resp.Header)
	case http.StatusRequestedRangeNotSatisfiable:
		// The range starts past the end of what the server has (it answers bytes */<size>).
		why := "the server rejected the resume range"
		if total := parseTotalOnly(resp.Header.Get("Content-Range")); total >= 0 {
			why = fmt.Sprintf("the server has %d bytes but the resume offset is %d", total, have)
		}
		return false, restartDownload(part, st, why)
	default:
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return false, errors.New(resp.Status)
		}
		return false, &permanentError{"the download answered " + resp.Status}
	}
	f, err := os.OpenFile(part, flags, 0o600)
	if err != nil {
		return false, &permanentError{err.Error()}
	}
	// One byte past the expected size proves the body is longer than the export says.
	n, err := io.Copy(f, io.LimitReader(&progressReader{r: resp.Body, base: have, pr: pr}, info.SizeBytes-have+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if have+n > info.SizeBytes {
		_ = os.Remove(part)
		return false, &permanentError{"the download is larger than the export's size"}
	}
	if err != nil {
		return false, err
	}
	return have+n == info.SizeBytes, nil
}

// dlState is what the first response of a download says about the object, kept across
// attempts (and fresh presigned urls) to guard each resume.
type dlState struct{ validator string }

// validatorOf is the strong ETag, else the Last-Modified, of a response; If-Range takes either.
func validatorOf(h http.Header) string {
	if e := h.Get("ETag"); e != "" && !strings.HasPrefix(e, "W/") {
		return e
	}
	return h.Get("Last-Modified")
}

// restartDownload discards the partial file so the next attempt starts from zero.
func restartDownload(part string, st *dlState, why string) error {
	st.validator = ""
	if err := os.Truncate(part, 0); err != nil && !os.IsNotExist(err) {
		return &permanentError{err.Error()}
	}
	return fmt.Errorf("%s; restarting from zero", why)
}

// badContentRange checks a 206 against the open-ended range asked for from offset have. It
// returns why the response cannot be appended, or "" when it is exactly the rest of the export.
func badContentRange(h http.Header, have, size int64) string {
	var start, end, total int64
	var star string
	cr := strings.TrimSpace(h.Get("Content-Range"))
	if n, _ := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); n == 3 {
		if start != have || end != size-1 || total != size {
			return fmt.Sprintf("the server answered %q for a resume at %d of %d", cr, have, size)
		}
		return ""
	}
	if n, _ := fmt.Sscanf(cr, "bytes %d-%d/%1s", &start, &end, &star); n == 3 && star == "*" {
		if start != have || end != size-1 {
			return fmt.Sprintf("the server answered %q for a resume at %d of %d", cr, have, size)
		}
		return ""
	}
	return fmt.Sprintf("the server answered a range without a usable Content-Range (%q)", cr)
}

// parseTotalOnly reads the size out of "bytes */<size>" (the 416 form), or -1.
func parseTotalOnly(cr string) int64 {
	var total int64
	if n, _ := fmt.Sscanf(strings.TrimSpace(cr), "bytes */%d", &total); n == 1 {
		return total
	}
	return -1
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// exportRequest builds the GET for an export url. A relative url is the control plane's own
// download route and carries the API key; an absolute one is presigned and carries nothing.
func exportRequest(ctx context.Context, cl *Client, raw string) (*http.Request, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the export url is not valid")
	}
	if u.IsAbs() {
		return http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	}
	if !strings.HasPrefix(raw, "/") || u.Host != "" {
		return nil, errors.New("the export url is not valid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cl.BaseURL, "/")+raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cl.Key)
	return req, nil
}

type progressReader struct {
	r    io.Reader
	base int64
	n    int64
	pr   *progress
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	p.pr.update(p.base+p.n, false)
	return n, err
}
