package idem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRecordAndReplay(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Lookup("tok-1"); ok {
		t.Fatal("unknown token must miss")
	}
	if err := j.Record("tok-1", json.RawMessage(`{"ip":"10.0.0.2"}`)); err != nil {
		t.Fatal(err)
	}
	res, ok := j.Lookup("tok-1")
	if !ok || string(res.Response) != `{"ip":"10.0.0.2"}` {
		t.Fatalf("replay = %+v,%v", res, ok)
	}
}

func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record("tok-9", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.Lookup("tok-9"); !ok {
		t.Fatal("reopened journal must replay the token")
	}
}

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestExpiredEntriesDroppedOnOpen(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	j, err := OpenWith(dir, Config{TTL: time.Hour, Now: fixedClock(&now)})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Record("old", json.RawMessage(`{"a":1}`))
	now = now.Add(30 * time.Minute)
	_ = j.Record("new", json.RawMessage(`{"b":2}`))
	now = now.Add(40 * time.Minute)
	if _, ok := j.Lookup("old"); ok {
		t.Fatal("expired token must miss")
	}
	re, err := OpenWith(dir, Config{TTL: time.Hour, Now: fixedClock(&now)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := re.Lookup("new"); !ok {
		t.Fatal("live token lost")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "tokens.jsonl"))
	if strings.Contains(string(raw), `"old"`) || !strings.Contains(string(raw), `"new"`) {
		t.Fatalf("compaction wrong: %s", raw)
	}
}

func TestLegacyEntriesKeptAndLargeOnesScrubbed(t *testing.T) {
	dir := t.TempDir()
	big := `{"stdout":"` + strings.Repeat("s", MaxResponseBytes) + `"}`
	legacy := `{"token":"small","response":{"ok":1}}` + "\n" + `{"token":"big","response":` + big + "}\nnot json\n"
	if err := os.WriteFile(filepath.Join(dir, "tokens.jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := j.Lookup("small"); !ok || string(e.Response) != `{"ok":1}` {
		t.Fatalf("legacy small = %+v,%v", e, ok)
	}
	if e, ok := j.Lookup("big"); !ok || e.Marker == nil || e.Response != nil || e.Marker.Bytes != len(big) {
		t.Fatalf("legacy big = %+v,%v", e, ok)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "tokens.jsonl"))
	if strings.Contains(string(raw), "ssss") {
		t.Fatal("large payload still on disk")
	}
}

func TestLargeAndMarkerRecordsKeepNoPayload(t *testing.T) {
	dir := t.TempDir()
	j, _ := Open(dir)
	big := json.RawMessage(`{"d":"` + strings.Repeat("x", MaxResponseBytes) + `"}`)
	if err := j.Record("big", big); err != nil {
		t.Fatal(err)
	}
	if err := j.RecordMarker("exec", json.RawMessage(`{"stdout":"SECRET"}`)); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"big", "exec"} {
		if e, ok := j.Lookup(tok); !ok || e.Marker == nil || e.Marker.SHA256 == "" || e.Response != nil {
			t.Fatalf("%s = %+v,%v", tok, e, ok)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "tokens.jsonl"))
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "xxxx") {
		t.Fatal("payload persisted")
	}
}

func TestCompactsWhenDueAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	j, _ := OpenWith(dir, Config{TTL: time.Minute, Now: fixedClock(&now)})
	_ = j.Record("a", json.RawMessage(`1`))
	now = now.Add(2 * time.Hour)
	_ = j.Record("b", json.RawMessage(`2`))
	raw, _ := os.ReadFile(filepath.Join(dir, "tokens.jsonl"))
	if strings.Contains(string(raw), `"a"`) {
		t.Fatalf("periodic compaction did not run: %s", raw)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, "tokens.jsonl")); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", st.Mode().Perm())
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

func TestOpenRemovesStaleTempAndKeepsEntries(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "tokens-123.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.jsonl"), []byte(`{"token":"t","response":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp file kept")
	}
	if _, ok := j.Lookup("t"); !ok {
		t.Fatal("entry lost")
	}
}

func TestOpenContinuesWhenCompactionFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.jsonl")
	if err := os.WriteFile(path, []byte(`{"token":"t","response":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions not enforced")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	j, err := Open(dir)
	if err != nil {
		t.Fatalf("startup must survive a failed rewrite: %v", err)
	}
	if _, ok := j.Lookup("t"); !ok {
		t.Fatal("entry lost")
	}
}

func TestSizeCompactionScalesWithLiveSet(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	j, _ := OpenWith(dir, Config{Now: fixedClock(&now)})
	j.live = 10 * compactBytes
	j.lastCompact = now
	j.size = 5 * compactBytes
	_ = j.Record("b", json.RawMessage(`1`))
	if j.size < 5*compactBytes {
		t.Fatal("compacted although file is under twice the live set")
	}
}
