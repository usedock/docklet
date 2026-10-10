// Package idem journals idempotency tokens to disk so a retried docklet
// call executes once even across restarts. Small responses replay
// byte-identical; large or guest-derived ones keep only a hash marker.
package idem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultTTL is how long a token stays deduplicated.
	DefaultTTL = 7 * 24 * time.Hour
	// MaxResponseBytes caps a journaled response; larger ones keep a marker.
	MaxResponseBytes = 64 << 10
	// compactBytes triggers a compaction when the file grows past it.
	compactBytes = 4 << 20
	// compactEvery bounds how long the file goes without compaction.
	compactEvery = time.Hour
)

// Config tunes a journal; zero values select the defaults.
type Config struct {
	TTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Entry is one recorded token. Marker entries carry no response body.
type Entry struct {
	Response json.RawMessage
	Marker   *Marker
}

// Marker proves a token executed without retaining its (large) result.
type Marker struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type record struct {
	Token    string          `json:"token"`
	TS       int64           `json:"ts,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	Marker   *Marker         `json:"marker,omitempty"`
}

// Journal is a token log with an in-memory index and periodic compaction.
type Journal struct {
	mu          sync.Mutex
	path        string
	ttl         time.Duration
	now         func() time.Time
	found       map[string]record
	size        int64
	live        int64
	lastCompact time.Time
}

// Open loads the journal with default settings.
func Open(dir string) (*Journal, error) { return OpenWith(dir, Config{}) }

// OpenWith loads the journal, drops expired entries and rewrites the file.
func OpenWith(dir string, cfg Config) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, "tokens.jsonl"), ttl: cfg.TTL, now: cfg.Now, found: map[string]record{}}
	if j.ttl <= 0 {
		j.ttl = DefaultTTL
	}
	if j.now == nil {
		j.now = time.Now
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "tokens-*.tmp"))
	for _, name := range stale {
		_ = os.Remove(name)
	}
	raw, err := os.ReadFile(j.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	now := j.now()
	for _, line := range splitLines(raw) {
		var rec record
		if json.Unmarshal(line, &rec) != nil || rec.Token == "" {
			continue
		}
		if rec.TS == 0 {
			// Pre-TTL entries age from first load so live tokens are not lost.
			rec.TS = now.Unix()
		}
		j.found[rec.Token] = shrink(rec)
	}
	if err := j.compact(); err != nil {
		// A failed rewrite (e.g. disk full) must not block startup; the loaded index still works.
		log.Printf("docklet: idempotency journal compaction failed, continuing: %v", err)
	}
	return j, nil
}

// shrink replaces an oversized response with a marker so it is never kept.
func shrink(rec record) record {
	if rec.Marker == nil && len(rec.Response) > MaxResponseBytes {
		rec.Marker = markerOf(rec.Response)
		rec.Response = nil
	}
	return rec
}

func markerOf(response []byte) *Marker {
	sum := sha256.Sum256(response)
	return &Marker{SHA256: hex.EncodeToString(sum[:]), Bytes: len(response)}
}

func (j *Journal) expired(rec record) bool {
	return j.now().Sub(time.Unix(rec.TS, 0)) > j.ttl
}

// Lookup returns the recorded entry for a seen, unexpired token.
func (j *Journal) Lookup(token string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec, ok := j.found[token]
	if !ok || j.expired(rec) {
		return Entry{}, false
	}
	return Entry{Response: rec.Response, Marker: rec.Marker}, true
}

// Record journals a response, or only a marker when it exceeds the size cap.
func (j *Journal) Record(token string, response json.RawMessage) error {
	return j.append(shrink(record{Token: token, Response: response}))
}

// RecordMarker journals only a hash marker, for results that must not persist.
func (j *Journal) RecordMarker(token string, response json.RawMessage) error {
	return j.append(record{Token: token, Marker: markerOf(response)})
}

// append writes one entry with an fsync, then compacts when due.
func (j *Journal) append(rec record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	rec.TS = j.now().Unix()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	j.found[rec.Token] = rec
	j.size += int64(len(line))
	if j.size > max(compactBytes, 2*j.live) || j.now().Sub(j.lastCompact) > compactEvery {
		// The entry is already durable; a failed compaction retries later.
		_ = j.compact()
	}
	return nil
}

// compact drops expired entries and atomically rewrites the file (0600).
func (j *Journal) compact() error {
	live := make([]record, 0, len(j.found))
	for tok, rec := range j.found {
		if j.expired(rec) {
			delete(j.found, tok)
			continue
		}
		live = append(live, rec)
	}
	sort.Slice(live, func(a, b int) bool {
		if live[a].TS != live[b].TS {
			return live[a].TS < live[b].TS
		}
		return live[a].Token < live[b].Token
	})
	var buf []byte
	for _, rec := range live {
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	if err := writeAtomic(j.path, buf); err != nil {
		return err
	}
	j.size = int64(len(buf))
	j.live = j.size
	j.lastCompact = j.now()
	return nil
}

// writeAtomic replaces path via temp file, fsync, rename and directory fsync.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "tokens-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				lines = append(lines, raw[start:i])
			}
			start = i + 1
		}
	}
	return lines
}
