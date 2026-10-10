package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContextLifecycle(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "context", "ls")
	if !strings.Contains(te.out.String(), CloudURL) || !strings.Contains(te.out.String(), "*") {
		t.Fatalf("out %q", te.out)
	}
	mustExit(t, te, 0, "context", "add", "self", "--url", "http://localhost:3000/")
	mustExit(t, te, 0, "context", "ls", "--json")
	rows := jsonOut(t, te).([]any)
	if len(rows) != 2 || rows[1].(map[string]any)["url"] != "http://localhost:3000" {
		t.Fatalf("rows %v", rows)
	}
	mustExit(t, te, 0, "context", "use", "self")
	mustExit(t, te, 0, "context", "ls", "--json")
	rows = jsonOut(t, te).([]any)
	if rows[0].(map[string]any)["current"] != false || rows[1].(map[string]any)["current"] != true {
		t.Fatalf("rows %v", rows)
	}
	mustExit(t, te, 1, "context", "use", "ghost")
	mustExit(t, te, 2, "context", "add", "x")
	mustExit(t, te, 1, "context", "add", "x", "--url", "ftp://nope")
	mustExit(t, te, 1, "context", "add", "cloud", "--url", "http://x.test")
	mustExit(t, te, 1, "context", "add", "bad name", "--url", "http://x.test")
	mustExit(t, te, 1, "context", "rm", "cloud")
	mustExit(t, te, 0, "context", "rm", "self")
	mustExit(t, te, 0, "context", "ls", "--json")
	rows = jsonOut(t, te).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["current"] != true {
		t.Fatalf("removing the current context falls back to cloud: %v", rows)
	}
	mustExit(t, te, 1, "context", "rm", "self")
	mustExit(t, te, 2, "context")
}

func TestContextSelectsURLAndKey(t *testing.T) {
	te := newEnv(t)
	delete(te.env, "DOCK_API_URL")
	delete(te.env, "DOCK_API_KEY")
	mustExit(t, te, 0, "context", "add", "fake", "--url", te.api.srv.URL)
	mustExit(t, te, 0, "context", "use", "fake")
	te.app.Stdin = strings.NewReader("dock_stored_key\n")
	mustExit(t, te, 0, "login")
	mustExit(t, te, 0, "ls")
	if te.api.last().Auth != "Bearer dock_stored_key" {
		t.Fatalf("auth %q", te.api.last().Auth)
	}
}

func TestLoginKeyNeverPrinted(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "context", "add", "self", "--url", "http://localhost:1")
	te.app.Stdin = strings.NewReader("  dock_secret123  \n")
	mustExit(t, te, 0, "login", "--context", "self")
	mustExit(t, te, 0, "context", "ls")
	mustExit(t, te, 0, "context", "ls", "--json")
	if strings.Contains(te.out.String()+te.errb.String(), "secret123") {
		t.Fatal("API key leaked into output")
	}
	te.app.Stdin = strings.NewReader("dock_secret123\n")
	mustExit(t, te, 0, "login", "--context", "self", "--json")
	if strings.Contains(te.out.String(), "secret123") {
		t.Fatal("API key leaked into login output")
	}
	b, err := os.ReadFile(te.cfgPath)
	if err != nil || !strings.Contains(string(b), "dock_secret123") {
		t.Fatalf("key should be stored: %v", err)
	}
	// request failures never echo the key
	te.env["DOCK_API_URL"] = "http://127.0.0.1:1"
	te.env["DOCK_API_KEY"] = "dock_envsecret"
	mustExit(t, te, 1, "ls")
	if strings.Contains(te.errb.String(), "envsecret") {
		t.Fatal("key leaked in error")
	}
}

func TestLoginErrors(t *testing.T) {
	te := newEnv(t)
	te.app.Stdin = strings.NewReader("")
	mustExit(t, te, 1, "login")
	te.app.Stdin = strings.NewReader("two words\n")
	mustExit(t, te, 1, "login")
	te.app.Stdin = strings.NewReader("k\n")
	mustExit(t, te, 1, "login", "--context", "ghost")
	te.app.Stdin = strings.NewReader("dock_k\n")
	mustExit(t, te, 0, "login") // built-in cloud
	cfg, _ := LoadConfig(te.cfgPath)
	if cfg.Contexts[CloudContext].APIKey != "dock_k" {
		t.Fatal("cloud key not stored")
	}
	if c, _ := cfg.lookup(CloudContext); c.URL != CloudURL {
		t.Fatalf("cloud url %q", c.URL)
	}
}

func TestEnvOverrides(t *testing.T) {
	te := newEnv(t)
	other := newFakeAPI(t)
	mustExit(t, te, 0, "context", "add", "stored", "--url", "http://127.0.0.1:1")
	mustExit(t, te, 0, "context", "use", "stored")
	te.app.Stdin = strings.NewReader("dock_file\n")
	mustExit(t, te, 0, "login")
	// env URL and key win over the stored context
	mustExit(t, te, 0, "ls")
	if te.api.last().Auth != "Bearer dock_testkey" {
		t.Fatalf("auth %q", te.api.last().Auth)
	}
	// only the key overridden: the stored URL is unreachable
	te.env["DOCK_API_URL"] = other.srv.URL
	delete(te.env, "DOCK_API_KEY")
	mustExit(t, te, 0, "ls")
	if other.last().Auth != "Bearer dock_file" {
		t.Fatalf("auth %q", other.last().Auth)
	}
	te.env["DOCK_API_URL"] = "not a url"
	mustExit(t, te, 1, "ls")
}

func TestConfigFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	te := newEnv(t)
	te.app.Stdin = strings.NewReader("dock_k\n")
	mustExit(t, te, 0, "login")
	fi, err := os.Stat(te.cfgPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v err %v", fi, err)
	}
	di, _ := os.Stat(filepath.Dir(te.cfgPath))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	// a widened file is tightened again on the next write
	if err := os.Chmod(te.cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExit(t, te, 0, "context", "add", "x", "--url", "http://x.test")
	fi, _ = os.Stat(te.cfgPath)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v after rewrite", fi.Mode().Perm())
	}
}

func TestCorruptConfig(t *testing.T) {
	te := newEnv(t)
	if err := os.MkdirAll(filepath.Dir(te.cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(te.cfgPath, []byte("current = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	mustExit(t, te, 1, "ls")
	if !strings.HasPrefix(te.errb.String(), "error: config_error:") {
		t.Fatalf("err %q", te.errb)
	}
}
