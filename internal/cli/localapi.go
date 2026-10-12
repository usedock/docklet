package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/usedock/docklet/internal/local"
)

// localAPI answers the same /docks calls as the control plane API from the local engine,
// so command code and --json output are shared between the cloud and local contexts.
type localAPI struct{ e *local.Engine }

// localError maps an engine error to the CLI error type.
func localError(err error) error {
	if err == nil {
		return nil
	}
	var le *local.Error
	if errors.As(err, &le) {
		return newError(le.Code, le.Message)
	}
	return err
}

func (s dockSpec) localSpec() local.Spec {
	return local.Spec{Name: s.Name, Image: s.Image, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB}
}

func (l localAPI) Do(ctx context.Context, method, path string, _ map[string]string, body any) (json.RawMessage, error) {
	raw, err := l.do(ctx, method, path, body)
	return raw, localError(err)
}

func (l localAPI) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	path, query, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
	parts := strings.Split(path, "/")
	var in struct {
		Name           string `json:"name"`
		Image          string `json:"image"`
		Network        string `json:"network"`
		Runtime        string `json:"runtime"`
		Command        string `json:"command"`
		Cwd            string `json:"cwd"`
		CPUs           int    `json:"cpus"`
		MemoryMiB      int    `json:"memoryMiB"`
		DiskGiB        int    `json:"diskGiB"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
		Force          bool   `json:"force"`
	}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &in); err != nil {
			return nil, err
		}
	}
	switch {
	case parts[0] == "docks" && len(parts) == 1 && method == http.MethodGet:
		docks, err := l.e.List(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(query, "all=true") {
			running := docks[:0]
			for _, d := range docks {
				if d.State != "archived" {
					running = append(running, d)
				}
			}
			docks = running
		}
		return json.Marshal(docks)
	case parts[0] == "docks" && len(parts) == 1 && method == http.MethodPost:
		d, err := l.e.New(ctx, local.Spec{Name: in.Name, Image: in.Image, Network: in.Network, Runtime: in.Runtime, CPUs: in.CPUs, MemoryMiB: in.MemoryMiB, DiskGiB: in.DiskGiB})
		if err != nil {
			return nil, err
		}
		return json.Marshal(d)
	case parts[0] == "docks" && len(parts) == 2 && method == http.MethodDelete:
		if err := l.e.Remove(ctx, parts[1]); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"removed": true, "id": parts[1]})
	case parts[0] == "docks" && len(parts) == 3 && method == http.MethodPost:
		switch parts[2] {
		case "stop":
			d, err := l.e.Stop(ctx, parts[1], in.Force)
			if err != nil {
				return nil, err
			}
			return json.Marshal(d)
		case "resume":
			d, err := l.e.Resume(ctx, parts[1])
			if err != nil {
				return nil, err
			}
			return json.Marshal(d)
		case "exec":
			script := in.Command
			if in.Cwd != "" {
				script = "cd " + shellQuote(in.Cwd) + " && " + script
			}
			res, err := l.e.Exec(ctx, parts[1], script, time.Duration(in.TimeoutSeconds)*time.Second)
			if err != nil {
				return nil, err
			}
			return json.Marshal(res)
		}
	}
	return nil, &local.Error{Code: "not_supported", Message: method + " /" + path + " is not available for local docks"}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
