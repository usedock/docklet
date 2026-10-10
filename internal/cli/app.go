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

	"golang.org/x/term"
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

const usageText = `dock - command line client for Dock (early, not released yet)

Usage: dock <command> [flags]

Commands:
  new [name]            create a dock (reads ./dock.toml)
  ls                    list docks
  exec <dock> -- <cmd>  run a command in a dock
  ssh <dock>            open an ssh session through the Dock gateway
  port <dock> <port>    expose a dock port at a public URL
  stop <dock>           stop a dock
  resume <dock>         resume a stopped dock
  branch <dock>         start independent docks from a dock's state
  rm <dock>             delete a dock
  login                 store an API key for a context (read from stdin)
  context add|use|ls|rm manage control plane contexts (and a default workspace)

Every command accepts --json. <dock> is a dock name or id.
Environment: DOCK_API_URL, DOCK_API_KEY and DOCK_WORKSPACE override the active context.
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

// client builds the API client from the active context and env overrides.
func (a *App) client() (*Client, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	name := cfg.current()
	cx, ok := cfg.lookup(name)
	if !ok {
		return nil, newError("context_not_found", fmt.Sprintf("context %q does not exist", name))
	}
	base, key := cx.URL, cx.APIKey
	if v := a.env("DOCK_API_URL"); v != "" {
		if base, err = normalizeURL(v); err != nil {
			return nil, err
		}
	}
	if v := a.env("DOCK_API_KEY"); v != "" {
		key = strings.TrimSpace(v)
	}
	if base == "" {
		return nil, newError("no_url", fmt.Sprintf("context %q has no url", name))
	}
	if key == "" {
		return nil, newError("no_api_key", "no API key; run 'dock login' or set DOCK_API_KEY")
	}
	ws := cx.Workspace
	if v := a.env("DOCK_WORKSPACE"); v != "" {
		ws = strings.TrimSpace(v)
	}
	return &Client{BaseURL: base, Key: key, HTTP: a.HTTP, Workspace: ws}, nil
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
