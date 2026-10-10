package v1

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// fixture binds one testdata file to the exported type it must decode into.
type fixture struct {
	typeName string
	file     string
	newValue func() any
}

// fixtures must list every exported struct in this package at least once.
var fixtures = []fixture{
	{"Envelope", "envelope-ok.json", func() any { return new(Envelope) }},
	{"Envelope", "envelope-capacity-exhausted.json", func() any { return new(Envelope) }},
	{"Resources", "resources.json", func() any { return new(Resources) }},
	{"ProvisionRequest", "provision-request.json", func() any { return new(ProvisionRequest) }},
	{"ProvisionResponse", "provision-response.json", func() any { return new(ProvisionResponse) }},
	{"PauseRequest", "pause-request.json", func() any { return new(PauseRequest) }},
	{"SnapshotCreateRequest", "snapshot-create-request.json", func() any { return new(SnapshotCreateRequest) }},
	{"SnapshotCreateResponse", "snapshot-create-response.json", func() any { return new(SnapshotCreateResponse) }},
	{"SnapshotLoadRequest", "snapshot-load-request.json", func() any { return new(SnapshotLoadRequest) }},
	{"SnapshotLoadResponse", "snapshot-load-response.json", func() any { return new(SnapshotLoadResponse) }},
	{"DestroyRequest", "destroy-request.json", func() any { return new(DestroyRequest) }},
	{"FastRunCapabilities", "fast-run-capabilities.json", func() any { return new(FastRunCapabilities) }},
	{"FastRunCreateRequest", "fast-run-create-request.json", func() any { return new(FastRunCreateRequest) }},
	{"FastRunCreateResponse", "fast-run-create-response.json", func() any { return new(FastRunCreateResponse) }},
	{"FastRunExecRequest", "fast-run-exec-request.json", func() any { return new(FastRunExecRequest) }},
	{"FastRunExecResponse", "fast-run-exec-response.json", func() any { return new(FastRunExecResponse) }},
	{"FastRunReadRequest", "fast-run-read-request.json", func() any { return new(FastRunReadRequest) }},
	{"FastRunReadResponse", "fast-run-read-response.json", func() any { return new(FastRunReadResponse) }},
	{"FastRunTerminateRequest", "fast-run-terminate-request.json", func() any { return new(FastRunTerminateRequest) }},
	{"ContainerSample", "container-sample.json", func() any { return new(ContainerSample) }},
	{"StatsSample", "stats-sample.json", func() any { return new(StatsSample) }},
	{"RunningVM", "running-vm.json", func() any { return new(RunningVM) }},
	{"HeartbeatRequest", "heartbeat-request.json", func() any { return new(HeartbeatRequest) }},
	{"Readiness", "readiness-ready.json", func() any { return new(Readiness) }},
	{"Readiness", "readiness-not-ready.json", func() any { return new(Readiness) }},
}

func TestFixturesRoundTrip(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", f.file))
			if err != nil {
				t.Fatal(err)
			}
			v := f.newValue()
			if got := reflect.TypeOf(v).Elem().Name(); got != f.typeName {
				t.Fatalf("table says %s but value is %s", f.typeName, got)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatalf("decode into %s: %v", f.typeName, err)
			}
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip mismatch for %s\nfixture: %s\nencoded: %s", f.file, raw, out)
			}
		})
	}
}

func TestEveryExportedStructHasFixture(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return filepath.Ext(fi.Name()) == ".go" && !bytes.HasSuffix([]byte(fi.Name()), []byte("_test.go"))
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, f := range fixtures {
		covered[f.typeName] = true
	}
	var missing []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts := spec.(*ast.TypeSpec)
					if _, isStruct := ts.Type.(*ast.StructType); isStruct && ts.Name.IsExported() && !covered[ts.Name.Name] {
						missing = append(missing, ts.Name.Name)
					}
				}
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("exported types without a fixture: %v", missing)
	}
}

func TestEveryFixtureFileIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, f := range fixtures {
		listed[f.file] = true
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if !listed[filepath.Base(p)] {
			t.Errorf("fixture %s is not in the table", p)
		}
	}
}
