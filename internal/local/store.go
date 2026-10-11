package local

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Record is one dock in docks.json. Runtime facts (token, port, pid) live in the dock's
// own directory so a crashed CLI never leaves them half-written in the shared file.
type Record struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Image     string    `json:"image"`
	ImageDir  string    `json:"imageDir"`
	CPUs      int       `json:"cpus"`
	MemoryMiB int       `json:"memoryMiB"`
	DiskGiB   int       `json:"diskGiB"`
	Network   string    `json:"network"`
	Parent    string    `json:"parent,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Store is docks.json under Dir, guarded by a lock file across processes.
type Store struct {
	Dir string
	// LockTimeout bounds waiting for another process; zero means 10 seconds.
	LockTimeout time.Duration
}

const staleLock = 30 * time.Second

func (s *Store) file() string { return filepath.Join(s.Dir, "docks.json") }

// DockDir is where a dock's disk and runtime files live.
func (s *Store) DockDir(id string) string { return filepath.Join(s.Dir, "docks", id) }

// List reads the records without locking: writes replace the file atomically.
func (s *Store) List() ([]Record, error) {
	raw, err := os.ReadFile(s.file())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errf("store_error", "%v", err)
	}
	var recs []Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil, errf("store_error", "%s is not valid: %v", s.file(), err)
	}
	return recs, nil
}

// Update runs fn under the lock; the records fn leaves behind are saved when it returns nil.
func (s *Store) Update(fn func(recs *[]Record) error) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return errf("store_error", "%v", err)
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := s.List()
	if err != nil {
		return err
	}
	if err := fn(&recs); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".docks-*.json")
	if err != nil {
		return errf("store_error", "%v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return errf("store_error", "%v", err)
	}
	if err := tmp.Close(); err != nil {
		return errf("store_error", "%v", err)
	}
	if err := os.Rename(tmp.Name(), s.file()); err != nil {
		return errf("store_error", "%v", err)
	}
	return nil
}

// lock takes docks.lock with an exclusive create, which behaves the same on every OS.
// A lock older than staleLock belongs to a crashed process and is removed.
func (s *Store) lock() (func(), error) {
	path := filepath.Join(s.Dir, "docks.lock")
	wait := s.LockTimeout
	if wait == 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return nil, errf("store_error", "%v", err)
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleLock {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errf("store_locked", "another dock command holds %s; try again", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

// NewID returns a dock id in the cloud's dk_ style.
func NewID() string { return "dk_" + randomHex(8) }

// NewToken returns a guest agent token.
func NewToken() string { return randomHex(32) }
