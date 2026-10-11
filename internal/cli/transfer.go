package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	downloadAttempts = 6
	defaultBackoff   = time.Second
	defaultPoll      = 2 * time.Second
	pushReadyTimeout = 10 * time.Minute
	exportTimeout    = 30 * time.Minute
)

// contractMessages are readable texts for the error codes of the push and pull contract.
var contractMessages = map[string]string{
	"import_too_large":         "the disk is larger than this control plane accepts for an import",
	"import_checksum_mismatch": "the control plane received different bytes than were sent (checksum mismatch); try again",
	"runtime_unsupported":      "the control plane has no microVM host to run an imported dock; push needs a microVM host",
	"export_base_unsupported":  "this dock cannot be pulled: its base is not a Dockyard image (only docks built from a Dockyard image can move)",
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
	// No overall timeout: parts are large; every request carries the command's context.
	return &http.Client{}
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
	c := a.flags("push", "push <dock> --to <context> [--name N] [--workspace ID] [--stop] [--rm]")
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

	flat, err := eng.Flatten(ctx, pos[0], dir, *stop)
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
	body := map[string]any{"workspaceId": wsID, "image": rec.Image, "cpus": rec.CPUs, "memoryMiB": rec.MemoryMiB,
		"diskGiB": rec.DiskGiB, "sizeBytes": size, "sha256": sum}
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
	raw, err = cl.Do(ctx, "POST", "/docks/import/"+url.PathEscape(plan.ImportID)+"/complete", nil, map[string]any{"parts": parts})
	if err != nil {
		return contractError(err)
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
		for attempt := 1; ; attempt++ {
			etag, err = a.putPart(ctx, cl, target, direct, io.NewSectionReader(f, off, length), length)
			if err == nil {
				break
			}
			var perm *permanentError
			if errors.As(err, &perm) || ctx.Err() != nil || attempt >= partAttempts {
				return nil, newError("upload_failed", fmt.Sprintf("part %d of %d: %v", i, n, err))
			}
			if err := sleepCtx(ctx, a.backoff()<<(attempt-1)); err != nil {
				return nil, err
			}
		}
		out = append(out, donePart{Number: i, ETag: etag})
		pr.update(off+length, i == n)
	}
	return out, nil
}

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
	resp, err := a.transferHTTP().Do(req)
	if err != nil {
		return "", errors.New(redact(err.Error(), cl.Key))
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
		case "ready", "running":
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
	SHA256    string `json:"sha256"`
	BaseImage string `json:"baseImage"`
	URL       string `json:"url"`
}

func (a *App) cmdPull(ctx context.Context, args []string) error {
	c := a.flags("pull", "pull <dock> --from <context> [--name N]")
	from := c.fs.String("from", "", "context to pull from (a control plane, not local)")
	name := c.fs.String("name", "", "name of the new local dock (default: the remote name)")
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
	// Fail on a taken name before the long export and download.
	if err := eng.CheckName(localName); err != nil {
		return localError(err)
	}

	raw, err = cl.Do(ctx, "POST", "/docks/"+url.PathEscape(id)+"/export", nil, map[string]any{})
	if err != nil {
		return contractError(err)
	}
	var started struct {
		ExportID string `json:"exportId"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.ExportID == "" {
		return newError("bad_response", "unexpected export response")
	}
	if !*c.json {
		fmt.Fprintln(a.Stderr, "exporting the dock")
	}
	info, err := a.waitExport(ctx, cl, started.ExportID)
	if err != nil {
		return err
	}
	if info.BaseImage == "" {
		return contractError(newError("export_base_unsupported", ""))
	}
	if !*c.json {
		fmt.Fprintf(a.Stderr, "preparing the %s image\n", info.BaseImage)
	}
	cached, err := eng.EnsureImage(ctx, info.BaseImage)
	if err != nil {
		return localError(err)
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
	max := int64(info.DiskGiB) << 30
	if max <= 0 {
		max = local.DefaultMaxRawBytes
	}
	rawDisk := filepath.Join(dir, "disk.raw")
	if err := local.DecompressFile(part, rawDisk, max); err != nil {
		return localError(err)
	}
	_ = os.Remove(part)
	d, err := eng.Import(ctx, local.Import{Name: localName, Image: info.BaseImage, Cached: cached, RawPath: rawDisk,
		CPUs: info.CPUs, MemoryMiB: info.MemoryMiB, DiskGiB: info.DiskGiB})
	if err != nil {
		return localError(err)
	}
	if *c.json {
		return a.printValue(d)
	}
	fmt.Fprintf(a.Stdout, "pulled %s from %s as %s (%s) state=%s\n", pos[0], *from, dock(d).label(), d.ID, d.State)
	return nil
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
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, a.backoff()<<(attempt-2)); err != nil {
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
		done, err := a.fetchRange(ctx, info, part, pr)
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
		if attempt >= downloadAttempts {
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
	sum, err := local.FileSHA256(part)
	if err != nil {
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
func (a *App) fetchRange(ctx context.Context, info exportInfo, part string, pr *progress) (bool, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return false, &permanentError{err.Error()}
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := a.transferHTTP().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		// The server ignored Range: start over.
		flags |= os.O_TRUNC
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		_ = os.Truncate(part, 0)
		return false, errors.New(resp.Status)
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
