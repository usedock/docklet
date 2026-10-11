package local

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreConcurrentUpdatesLoseNothing(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(recs *[]Record) error {
				*recs = append(*recs, Record{ID: fmt.Sprintf("dk_%d", i)})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	recs, err := s.List()
	if err != nil || len(recs) != 20 {
		t.Fatalf("want 20 records, got %d (%v)", len(recs), err)
	}
}

func TestStoreUpdateErrorKeepsTheFile(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	_ = s.Update(func(recs *[]Record) error { *recs = append(*recs, Record{ID: "dk_a"}); return nil })
	err := s.Update(func(recs *[]Record) error { *recs = nil; return errf("nope", "no") })
	if err == nil {
		t.Fatal("want the callback error")
	}
	if recs, _ := s.List(); len(recs) != 1 {
		t.Fatalf("a failed update must not write: %v", recs)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "docks.lock")); !os.IsNotExist(err) {
		t.Fatal("lock must be released after a failed update")
	}
}

func TestStoreLockWaitsThenTimesOut(t *testing.T) {
	s := &Store{Dir: t.TempDir(), LockTimeout: 150 * time.Millisecond}
	if err := os.WriteFile(filepath.Join(s.Dir, "docks.lock"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(*[]Record) error { return nil })
	if codeOf(err) != "store_locked" {
		t.Fatalf("a held lock must time out, got %v", err)
	}
}

func TestStoreBreaksAStaleLock(t *testing.T) {
	s := &Store{Dir: t.TempDir(), LockTimeout: time.Second}
	lock := filepath.Join(s.Dir, "docks.lock")
	if err := os.WriteFile(lock, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(*[]Record) error { return nil }); err != nil {
		t.Fatalf("a lock left by a crashed process must be broken: %v", err)
	}
}

func TestStoreListMissingFileIsEmpty(t *testing.T) {
	recs, err := (&Store{Dir: t.TempDir()}).List()
	if err != nil || len(recs) != 0 {
		t.Fatalf("%v %v", recs, err)
	}
}

func TestIDsAndTokens(t *testing.T) {
	if id := NewID(); len(id) != 3+16 || id[:3] != "dk_" {
		t.Fatalf("id %q", id)
	}
	if a, b := NewToken(), NewToken(); len(a) != 64 || a == b {
		t.Fatal("tokens must be 64 hex chars and unique")
	}
}

func TestDataDirPerOS(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		goos string
		env  map[string]string
		home string
		want string
	}{
		{"linux", map[string]string{"XDG_DATA_HOME": "/x"}, "/h", filepath.Join("/x", "dock")},
		{"linux", nil, "/h", filepath.Join("/h", ".local", "share", "dock")},
		{"darwin", nil, "/Users/a", filepath.Join("/Users/a", "Library", "Application Support", "dock")},
		{"windows", map[string]string{"LOCALAPPDATA": `C:\L`}, `C:\U`, filepath.Join(`C:\L`, "dock")},
		{"linux", map[string]string{"DOCK_DATA_DIR": "/d"}, "/h", "/d"},
	}
	for _, c := range cases {
		got, err := dataDirFor(c.goos, env(c.env), c.home)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v, want %q", c.goos, got, err, c.want)
		}
	}
	if _, err := dataDirFor("linux", env(nil), ""); err == nil {
		t.Error("no home and no override must fail")
	}
}
