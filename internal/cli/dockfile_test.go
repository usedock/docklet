package cli

import (
	"strings"
	"testing"
)

func TestDockTomlFileOnly(t *testing.T) {
	te := newEnv(t)
	te.writeDockToml(t, "name = \"proj\"\nimage = \"ubuntu-24.04\"\ncpus = 3\nmemory = \"6gb\"\ndisk = 30\nenvironment = \"staging\"\n")
	mustExit(t, te, 0, "new")
	b := te.api.find("POST", "/v1/docks").Body
	want := map[string]any{"name": "proj", "image": "ubuntu-24.04", "cpus": 3.0, "memoryMiB": 6144.0, "diskGiB": 30.0, "environment": "staging"}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, b[k], v)
		}
	}
}

func TestDockTomlIntegerMemoryIsMiB(t *testing.T) {
	te := newEnv(t)
	te.writeDockToml(t, "memory = 2048\n")
	mustExit(t, te, 0, "new")
	if te.api.find("POST", "/v1/docks").Body["memoryMiB"] != 2048.0 {
		t.Fatal("integer memory is MiB")
	}
}

func TestDockTomlFlagOnly(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "new", "solo", "--cpus", "8")
	b := te.api.find("POST", "/v1/docks").Body
	if b["name"] != "solo" || b["cpus"] != 8.0 {
		t.Fatalf("body %v", b)
	}
	if _, ok := b["memoryMiB"]; ok {
		t.Fatal("unset sizes are omitted so the API defaults apply")
	}
}

func TestDockTomlFlagsOverrideFile(t *testing.T) {
	te := newEnv(t)
	te.writeDockToml(t, "name = \"proj\"\ncpus = 3\nmemory = \"6gb\"\ndisk = 30\nenvironment = \"staging\"\n")
	mustExit(t, te, 0, "new", "other", "--cpus", "1", "--memory", "1gb", "--env", "prod")
	b := te.api.find("POST", "/v1/docks").Body
	if b["name"] != "other" || b["cpus"] != 1.0 || b["memoryMiB"] != 1024.0 || b["environment"] != "prod" || b["diskGiB"] != 30.0 {
		t.Fatalf("body %v", b)
	}
}

func TestDockTomlErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":    "name = \"a\"\nbogus = 1\n",
		"unknown table":  "[extra]\nx = 1\n",
		"bad memory":     "memory = \"lots\"\n",
		"bad disk":       "disk = \"1000mb\"\n",
		"wrong type":     "cpus = \"two\"\n",
		"negative cpus":  "cpus = -1\n",
		"bad size type":  "memory = true\n",
		"invalid syntax": "name = \n",
	}
	for name, body := range cases {
		te := newEnv(t)
		te.writeDockToml(t, body)
		if got := te.run("new"); got != 1 {
			t.Errorf("%s: exit %d, want 1", name, got)
		}
		if !strings.HasPrefix(te.errb.String(), "error: invalid_dock_toml:") {
			t.Errorf("%s: err %q", name, te.errb)
		}
		if len(te.api.reqs) != 0 {
			t.Errorf("%s: no request expected", name)
		}
	}
	te := newEnv(t)
	te.writeDockToml(t, "bogus = 1\n")
	te.run("new")
	if !strings.Contains(te.errb.String(), "bogus") {
		t.Fatalf("unknown key should be named: %q", te.errb)
	}
}
