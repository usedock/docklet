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
	"strings"
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
	{"SnapshotCreateRequest", "snapshot-create-request-full.json", func() any { return new(SnapshotCreateRequest) }},
	{"SnapshotCreateResponse", "snapshot-create-response-full.json", func() any { return new(SnapshotCreateResponse) }},
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
	{"FleetRegisterRequest", "fleet-register-request.json", func() any { return new(FleetRegisterRequest) }},
	{"FleetRegisterRequest", "fleet-register-request-verified-imports.json", func() any { return new(FleetRegisterRequest) }},
	{"FleetRegisterResponse", "fleet-register-response.json", func() any { return new(FleetRegisterResponse) }},
	{"FleetHeartbeatRequest", "fleet-heartbeat-request.json", func() any { return new(FleetHeartbeatRequest) }},
	{"FleetHeartbeatRequest", "fleet-heartbeat-request-gvisor.json", func() any { return new(FleetHeartbeatRequest) }},
	{"FleetHeartbeatRequest", "fleet-heartbeat-request-images.json", func() any { return new(FleetHeartbeatRequest) }},
	{"FleetPollResponse", "fleet-poll-response.json", func() any { return new(FleetPollResponse) }},
	{"FleetPollResponse", "fleet-poll-response-empty.json", func() any { return new(FleetPollResponse) }},
	{"FleetCommand", "fleet-command.json", func() any { return new(FleetCommand) }},
	{"FleetResultRequest", "fleet-result-ok.json", func() any { return new(FleetResultRequest) }},
	{"FleetResultRequest", "fleet-result-error.json", func() any { return new(FleetResultRequest) }},
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

// TestEveryFieldAppearsInAFixture catches a new omitempty field, which the
// round trip alone cannot: its zero value re-encodes to the same JSON.
func TestEveryFieldAppearsInAFixture(t *testing.T) {
	seen := map[string]bool{}
	types := map[string]reflect.Type{}
	for _, f := range fixtures {
		raw, err := os.ReadFile(filepath.Join("testdata", f.file))
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		typ := reflect.TypeOf(f.newValue()).Elem()
		markSeen(typ, doc, seen)
		collectStructs(typ, types)
	}
	var missing []string
	for name, typ := range types {
		for _, key := range jsonKeys(typ) {
			if !seen[name+"."+key] {
				missing = append(missing, name+"."+key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("fields no fixture sets: %v", missing)
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonFields maps each JSON key of a struct, embedded structs flattened, to its field.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if sf.Anonymous && tag == "" && deref(sf.Type).Kind() == reflect.Struct {
			for k, v := range jsonFields(deref(sf.Type)) {
				out[k] = v
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		if tag == "" {
			tag = sf.Name
		}
		out[tag] = sf
	}
	return out
}

func jsonKeys(t reflect.Type) []string {
	var keys []string
	for k := range jsonFields(t) {
		keys = append(keys, k)
	}
	return keys
}

func collectStructs(t reflect.Type, into map[string]reflect.Type) {
	t = deref(t)
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		collectStructs(t.Elem(), into)
	case reflect.Struct:
		if _, done := into[t.Name()]; done {
			return
		}
		into[t.Name()] = t
		for _, sf := range jsonFields(t) {
			collectStructs(sf.Type, into)
		}
	}
}

func markSeen(t reflect.Type, doc any, seen map[string]bool) {
	t = deref(t)
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		if items, ok := doc.([]any); ok {
			for _, item := range items {
				markSeen(t.Elem(), item, seen)
			}
		}
	case reflect.Map:
		if m, ok := doc.(map[string]any); ok {
			for _, v := range m {
				markSeen(t.Elem(), v, seen)
			}
		}
	case reflect.Struct:
		m, ok := doc.(map[string]any)
		if !ok {
			return
		}
		for key, sf := range jsonFields(t) {
			if v, ok := m[key]; ok {
				seen[t.Name()+"."+key] = true
				markSeen(sf.Type, v, seen)
			}
		}
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
