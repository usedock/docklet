// Package uid allocates one unprivileged uid/gid per microVM dock from a
// reserved range, persisted so a dock keeps its identity across restarts and
// resumes. The jailer drops to this id, so a VMM escape lands as a user that
// owns nothing but its own dock's files.
package uid

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Defaults: far above login users (1000+), the default subuid ranges (from
// 100000, 65536 each) and the systemd container range (up to 1879048191).
const (
	DefaultBase = 2000000000
	DefaultSize = 65536
	maxUID      = 4294967293 // 2^32-2 is reserved
)

// Allocator hands out ids from [base, base+size), persisted as dock -> uid.
type Allocator struct {
	mu   sync.Mutex
	path string
	base int
	size int
	used map[string]int
}

// Open loads or creates the allocation file in dir.
func Open(dir string, base, size int) (*Allocator, error) {
	if err := ValidRange(base, size); err != nil {
		return nil, err
	}
	a := &Allocator{path: filepath.Join(dir, "uids.json"), base: base, size: size, used: map[string]int{}}
	raw, err := os.ReadFile(a.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.used); err != nil {
			return nil, fmt.Errorf("uid allocations: %w", err)
		}
	}
	return a, nil
}

// Assign returns the dock's id, allocating on first call: the preferred slot
// is a hash of the dock id, probing forward past ids held by other docks.
func (a *Allocator) Assign(dockID string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.used[dockID]; ok {
		return id, nil
	}
	taken := make(map[int]bool, len(a.used))
	for _, id := range a.used {
		taken[id] = true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(dockID))
	start := int(h.Sum32() % uint32(a.size))
	for i := 0; i < a.size; i++ {
		id := a.base + (start+i)%a.size
		if taken[id] {
			continue
		}
		a.used[dockID] = id
		if err := a.save(); err != nil {
			delete(a.used, dockID)
			return 0, err
		}
		return id, nil
	}
	return 0, fmt.Errorf("uid range %d+%d exhausted", a.base, a.size)
}

// Get returns the dock's id without allocating.
func (a *Allocator) Get(dockID string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.used[dockID]
	return id, ok
}

// Docks lists the dock ids holding an allocation.
func (a *Allocator) Docks() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.used))
	for id := range a.used {
		ids = append(ids, id)
	}
	return ids
}

// Release frees a dock's id for reuse.
func (a *Allocator) Release(dockID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.used[dockID]
	if !ok {
		return nil
	}
	delete(a.used, dockID)
	if err := a.save(); err != nil {
		a.used[dockID] = id
		return err
	}
	return nil
}

// fsync is a seam so tests can count durability calls.
var fsync = func(f *os.File) error { return f.Sync() }

// save writes temp, fsyncs it, renames, then fsyncs the dir so a crash never
// loses or tears an allocation.
func (a *Allocator) save() error {
	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(a.used)
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := fsync(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fsync(d)
}

// CheckRange refuses a range that overlaps a passwd user, a group (the dock's
// gid equals its uid) or a subuid/subgid range, so a dock id never equals a real
// account, group or userns mapping. Missing files mean nothing to clash with.
func CheckRange(base, size int, passwdPath, groupPath, subuidPath, subgidPath string) error {
	end := base + size - 1
	overlaps := func(lo, hi int) bool { return lo <= end && hi >= base }
	for _, p := range []string{passwdPath, groupPath} {
		if err := scan(p, 2, func(id, _ int) error {
			if overlaps(id, id) {
				return fmt.Errorf("uid range %d-%d contains id %d (%s)", base, end, id, p)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, p := range []string{subuidPath, subgidPath} {
		if err := scan(p, 1, func(start, count int) error {
			if count > 0 && overlaps(start, start+count-1) {
				return fmt.Errorf("uid range %d-%d overlaps subordinate range %d-%d (%s)", base, end, start, start+count-1, p)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// scan parses colon files: field idx is the first number; for subid files the
// next field is the count (passwd callers ignore it).
func scan(path string, idx int, fn func(a, b int) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(fields) <= idx || strings.HasPrefix(fields[0], "#") {
			continue
		}
		a, err := strconv.Atoi(fields[idx])
		if err != nil {
			continue
		}
		b := 0
		if idx == 1 && len(fields) > 2 {
			b, _ = strconv.Atoi(fields[2])
		}
		if err := fn(a, b); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ValidRange reports whether [base, base+size) is a usable id range.
func ValidRange(base, size int) error {
	if base < 1 || size < 1 || base+size-1 > maxUID {
		return fmt.Errorf("uid range base=%d size=%d is invalid", base, size)
	}
	return nil
}
