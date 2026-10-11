package cli

import (
	"strings"
	"testing"
)

const (
	wsOther  = "55555555-5555-4555-8555-555555555555"
	orgOther = "66666666-6666-4666-8666-666666666666"
)

func TestNewSingleWorkspaceIsAnnounced(t *testing.T) {
	te := newEnv(t)
	te.api.routes["GET /v1/organizations"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": orgID, "name": "acme"}}
	}
	te.api.routes["GET /v1/organizations/"+orgID+"/workspaces"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": wsID, "name": "Default"}}
	}
	mustExit(t, te, 0, "new", "api")
	if te.api.last().Body["workspaceId"] != wsID || !strings.Contains(te.errb.String(), "using workspace acme/Default ("+wsID+")") {
		t.Fatalf("body %v stderr %q", te.api.last().Body, te.errb)
	}
	mustExit(t, te, 0, "new", "api", "--json")
	if te.errb.Len() != 0 {
		t.Fatalf("--json should keep stderr quiet: %q", te.errb)
	}
}

func TestNewSeveralWorkspacesNeedsAChoice(t *testing.T) {
	te := newEnv(t)
	te.api.routes["GET /v1/organizations"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": orgID, "name": "acme"}, map[string]any{"id": orgOther, "name": "side"}}
	}
	te.api.routes["GET /v1/organizations/"+orgOther+"/workspaces"] = func(map[string]any) (int, any) {
		return 200, []any{map[string]any{"id": wsOther, "name": "Default"}}
	}
	mustExit(t, te, 1, "new", "api")
	if te.api.find("POST", "/v1/docks") != nil {
		t.Fatal("must not create a dock in a guessed workspace")
	}
	if e := te.errb.String(); !strings.Contains(e, "workspace_required") || !strings.Contains(e, wsID) || !strings.Contains(e, wsOther) {
		t.Fatalf("stderr %q", e)
	}

	// a context default resolves it without discovery
	mustExit(t, te, 0, "context", "use", "cloud", "--workspace", wsOther)
	mustExit(t, te, 0, "new", "api")
	if te.api.last().Body["workspaceId"] != wsOther {
		t.Fatalf("body %v", te.api.last().Body)
	}
	mustExit(t, te, 0, "context", "ls", "--json")
	if rows := jsonOut(t, te).([]any); rows[1].(map[string]any)["workspace"] != wsOther {
		t.Fatalf("context ls %v", rows)
	}
	// DOCK_WORKSPACE beats the context, --workspace beats both
	te.env["DOCK_WORKSPACE"] = wsID
	mustExit(t, te, 0, "new", "api")
	if te.api.last().Body["workspaceId"] != wsID {
		t.Fatalf("body %v", te.api.last().Body)
	}
	mustExit(t, te, 0, "new", "api", "--workspace", wsOther)
	if te.api.last().Body["workspaceId"] != wsOther {
		t.Fatalf("body %v", te.api.last().Body)
	}
	// an empty --workspace clears the default
	delete(te.env, "DOCK_WORKSPACE")
	mustExit(t, te, 0, "context", "use", "cloud", "--workspace", "")
	mustExit(t, te, 1, "new", "api")
}

func TestContextAddWorkspace(t *testing.T) {
	te := newEnv(t)
	mustExit(t, te, 0, "context", "add", "self", "--url", "http://localhost:1", "--workspace", wsOther)
	cfg, err := LoadConfig(te.cfgPath)
	if err != nil || cfg.Contexts["self"].Workspace != wsOther {
		t.Fatalf("cfg %+v %v", cfg, err)
	}
	// re-adding to change the url keeps the workspace
	mustExit(t, te, 0, "context", "add", "self", "--url", "http://localhost:2")
	if cfg, _ = LoadConfig(te.cfgPath); cfg.Contexts["self"].Workspace != wsOther {
		t.Fatalf("workspace lost: %+v", cfg.Contexts["self"])
	}
}

func TestLoginOnTerminalReadsWithoutEcho(t *testing.T) {
	te := newEnv(t)
	te.app.StdinIsTTY = true
	te.app.Stdin = strings.NewReader("must-not-be-read\n")
	te.app.ReadSecret = func() (string, error) { return "dock_hidden42", nil }
	mustExit(t, te, 0, "login")
	if strings.Contains(te.out.String()+te.errb.String(), "hidden42") {
		t.Fatal("API key leaked into output")
	}
	if !strings.Contains(te.errb.String(), "API key: ") {
		t.Fatalf("no prompt: %q", te.errb)
	}
	cfg, _ := LoadConfig(te.cfgPath)
	if cfg.Contexts[CloudContext].APIKey != "dock_hidden42" {
		t.Fatalf("stored %q", cfg.Contexts[CloudContext].APIKey)
	}
}

func TestExecSignalExitCode(t *testing.T) {
	te := newEnv(t)
	te.api.routes["POST /v1/docks/"+dockA+"/exec"] = func(map[string]any) (int, any) {
		return 200, map[string]any{"exitCode": -9, "stdout": "", "stderr": "", "durationMs": 1, "truncated": false}
	}
	mustExit(t, te, 137, "exec", dockA, "--", "sleep 100")
}
