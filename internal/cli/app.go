// Package cli implements the dock command line client for the Dock control plane /v1 API.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/usedock/docklet/internal/local"
)

// App is one CLI invocation environment; tests replace its fields.
type App struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	StdinIsTTY bool
	Getenv     func(string) string
	ConfigPath string // empty means DefaultConfigPath
	WorkDir    string // directory holding dock.toml; empty means the current directory
	HTTP       *http.Client
	// RunSSH runs the ssh client; nil uses os/exec.
	RunSSH func(ctx context.Context, argv []string) error
	// ReadSecret reads a line without echo from the terminal; nil reads a line from Stdin.
	ReadSecret func() (string, error)
	// Engine is the local engine; nil builds the real one on first use.
	Engine *local.Engine

	// xferBackoff and xferPoll override the push and pull waits (tests).
	xferBackoff, xferPoll time.Duration
	// xferIdle and xferHeaderWait override the transfer idle and header timeouts (tests).
	xferIdle, xferHeaderWait time.Duration
}

// NewApp returns an App wired to the process streams.
func NewApp() *App {
	fd := int(os.Stdin.Fd())
	a := &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, StdinIsTTY: term.IsTerminal(fd), Getenv: os.Getenv}
	if a.StdinIsTTY {
		a.ReadSecret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
	}
	return a
}

const usageText = `dock - run isolated Linux machines for agents on your laptop, your servers or Dock Cloud

Usage: dock <command> [flags]

Commands:
  new [name]            create a dock (reads ./dock.toml)
  ls                    list docks
  exec <dock> -- <cmd>  run a command in a dock
  ssh <dock>            open a shell (local: through the guest agent; cloud: the Dock SSH gateway)
  port <dock> <port>    cloud: expose a dock port at a public URL; local: forward it to 127.0.0.1
  stop <dock>           stop a dock
  resume <dock>         resume a stopped dock
  branch <dock>         start independent docks from a dock's state (local: a copy of its disk)
  rm <dock>             delete a dock
  push <dock> --to <ctx>    copy a stopped local dock's disk to a control plane (--name, --stop, --rm)
  pull <dock> --from <ctx>  copy a control plane dock's disk to a new stopped local dock (--name)
  images                list Dockyard images; 'images pull <name>' downloads one
  doctor                check what local docks need (QEMU, accelerator, disk, Dockyard)
  login                 store an API key for a context (read from stdin)
  context add|use|ls|rm manage contexts (and a default workspace)

Contexts: 'local' runs docks on this machine with QEMU and needs no account; it is the default
until you run 'dock login' or set DOCK_API_KEY. 'cloud' and added URLs use a control plane.
Switch with 'dock context use local|cloud|<name>'.

Every command accepts --json. <dock> is a dock name or id.
Environment: DOCK_API_URL, DOCK_API_KEY and DOCK_WORKSPACE override the active context.
Local: DOCK_QEMU (qemu-system binary), DOCK_DATA_DIR, DOCK_DOCKYARD_URL + DOCK_DOCKYARD_SHA256
(an unpinned image index), DOCK_LOCAL_ACCEL=tcg
(slow software emulation, tests only).
`

// Run executes args (without the program name) and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	err := a.run(ctx, args)
	var ue *usageError
	var ee *exitError
	var ae *Error
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelp):
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.As(err, &ue):
		fmt.Fprintf(a.Stderr, "error: %s\nRun 'dock help' for usage.\n", ue.msg)
		return 2
	case errors.As(err, &ae):
		fmt.Fprintf(a.Stderr, "error: %s: %s\n", ae.Code, ae.Message)
		return 1
	default:
		fmt.Fprintf(a.Stderr, "error: %v\n", err)
		return 1
	}
}

func (a *App) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("missing command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usageText)
		return nil
	case "new":
		return a.cmdNew(ctx, rest)
	case "ls", "list":
		return a.cmdLs(ctx, rest)
	case "exec":
		return a.cmdExec(ctx, rest)
	case "ssh":
		return a.cmdSSH(ctx, rest)
	case "port":
		return a.cmdPort(ctx, rest)
	case "stop":
		return a.cmdStop(ctx, rest)
	case "resume":
		return a.cmdResume(ctx, rest)
	case "branch":
		return a.cmdBranch(ctx, rest)
	case "rm":
		return a.cmdRm(ctx, rest)
	case "push":
		return a.cmdPush(ctx, rest)
	case "pull":
		return a.cmdPull(ctx, rest)
	case "images":
		return a.cmdImages(ctx, rest)
	case "doctor":
		return a.cmdDoctor(ctx, rest)
	case "login":
		return a.cmdLogin(rest)
	case "context":
		return a.cmdContext(rest)
	default:
		return usagef("unknown command %q", cmd)
	}
}

// cmdFlags is a flag set that always carries --json.
type cmdFlags struct {
	fs   *flag.FlagSet
	json *bool
	a    *App
	use  string
}

func (a *App) flags(name, use string) *cmdFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	j := fs.Bool("json", false, "print the API data as JSON")
	return &cmdFlags{fs: fs, json: j, a: a, use: use}
}

// parse accepts flags before, between and after positionals.
func (c *cmdFlags) parse(args []string, minPos, maxPos int) ([]string, error) {
	var pos []string
	for {
		if err := c.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(c.a.Stdout, "Usage: dock %s\n", c.use)
				c.fs.SetOutput(c.a.Stdout)
				c.fs.PrintDefaults()
				return nil, errHelp
			}
			return nil, usagef("%v", err)
		}
		args = c.fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) < minPos || len(pos) > maxPos {
		return nil, usagef("usage: dock %s", c.use)
	}
	return pos, nil
}

func (c *cmdFlags) isSet(name string) bool {
	set := false
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func (a *App) configPath() (string, error) {
	if a.ConfigPath != "" {
		return a.ConfigPath, nil
	}
	p, err := DefaultConfigPath()
	if err != nil {
		return "", newError("config_error", err.Error())
	}
	return p, nil
}

func (a *App) env(k string) string {
	if a.Getenv == nil {
		return ""
	}
	return a.Getenv(k)
}

// doer is what the commands talk to: the HTTP Client or the local engine, behind one call shape.
type doer interface {
	Do(ctx context.Context, method, path string, headers map[string]string, body any) (json.RawMessage, error)
}

// target is the active context's backend.
type target struct {
	doer
	// Workspace is the context's default workspace id; empty means discover it.
	Workspace string
	// Local is set for the local context.
	Local *local.Engine
}

// client builds the backend from the active context and env overrides.
func (a *App) client() (*target, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	name := cfg.active(a.Getenv)
	if name == LocalContext {
		e, err := a.engine()
		if err != nil {
			return nil, err
		}
		return &target{doer: localAPI{e}, Local: e}, nil
	}
	cl, ws, err := a.remote(cfg, name)
	if err != nil {
		return nil, err
	}
	return &target{doer: cl, Workspace: ws}, nil
}

// remote builds the HTTP client for a named control plane context. The DOCK_API_* and
// DOCK_WORKSPACE overrides apply only when name is the active context.
func (a *App) remote(cfg *Config, name string) (*Client, string, error) {
	if name == LocalContext {
		return nil, "", newError("invalid_argument", "the local context is not a control plane")
	}
	cx, ok := cfg.lookup(name)
	if !ok {
		return nil, "", newError("context_not_found", fmt.Sprintf("context %q does not exist", name))
	}
	base, key, ws := cx.URL, cx.APIKey, cx.Workspace
	if name == cfg.active(a.Getenv) {
		var err error
		if v := a.env("DOCK_API_URL"); v != "" {
			if base, err = normalizeURL(v); err != nil {
				return nil, "", err
			}
		}
		if v := a.env("DOCK_API_KEY"); v != "" {
			key = strings.TrimSpace(v)
		}
		if v := a.env("DOCK_WORKSPACE"); v != "" {
			ws = strings.TrimSpace(v)
		}
	}
	if base == "" {
		return nil, "", newError("no_url", fmt.Sprintf("context %q has no url", name))
	}
	if key == "" {
		return nil, "", newError("no_api_key", "no API key; run 'dock login' or set DOCK_API_KEY")
	}
	return &Client{BaseURL: base, Key: key, HTTP: a.HTTP}, ws, nil
}

// engine returns the local engine, building the real one on first use.
func (a *App) engine() (*local.Engine, error) {
	if a.Engine != nil {
		return a.Engine, nil
	}
	e, err := local.NewEngine(a.env, a.Stderr)
	if err != nil {
		return nil, localError(err)
	}
	a.Engine = e
	return e, nil
}

func (a *App) printJSON(raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := a.Stdout.Write(buf.Bytes())
	return err
}

func (a *App) printValue(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return a.printJSON(raw)
}
