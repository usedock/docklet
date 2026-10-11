package cli

import (
	"bufio"
	"fmt"
	"strings"
	"text/tabwriter"
)

func (a *App) cmdContext(args []string) error {
	const use = "context add <name> --url <url> [--workspace ID] | use <name> [--workspace ID] | ls | rm <name>"
	if len(args) == 0 {
		return usagef("usage: dock %s", use)
	}
	sub, rest := args[0], args[1:]
	path, err := a.configPath()
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		c := a.flags("context add", "context add <name> --url <url> [--workspace ID]")
		u := c.fs.String("url", "", "control plane URL")
		ws := c.fs.String("workspace", "", "default workspace id for dock new")
		pos, err := c.parse(rest, 1, 1)
		if err != nil {
			return err
		}
		if *u == "" {
			return usagef("usage: dock %s", c.use)
		}
		name := pos[0]
		if err := validateContextName(name); err != nil {
			return err
		}
		if name == CloudContext || name == LocalContext {
			return newError("invalid_argument", name+" is built in; use another name")
		}
		norm, err := normalizeURL(*u)
		if err != nil {
			return err
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		cx := cfg.Contexts[name]
		cx.URL = norm
		if c.isSet("workspace") {
			cx.Workspace = strings.TrimSpace(*ws)
		}
		cfg.Contexts[name] = cx
		if err := cfg.Save(path); err != nil {
			return err
		}
		if *c.json {
			return a.printValue(map[string]any{"name": name, "url": norm})
		}
		fmt.Fprintf(a.Stdout, "added context %s (%s)\n", name, norm)
		return nil
	case "use":
		c := a.flags("context use", "context use <name> [--workspace ID]")
		ws := c.fs.String("workspace", "", "default workspace id for dock new (empty clears it)")
		pos, err := c.parse(rest, 1, 1)
		if err != nil {
			return err
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if _, ok := cfg.lookup(pos[0]); !ok {
			return newError("context_not_found", fmt.Sprintf("context %q does not exist", pos[0]))
		}
		cfg.Current = pos[0]
		if c.isSet("workspace") {
			cx := cfg.Contexts[pos[0]]
			cx.Workspace = strings.TrimSpace(*ws)
			cfg.Contexts[pos[0]] = cx
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		if *c.json {
			return a.printValue(map[string]any{"current": pos[0]})
		}
		fmt.Fprintf(a.Stdout, "using context %s\n", pos[0])
		return nil
	case "ls", "list":
		c := a.flags("context ls", "context ls")
		if _, err := c.parse(rest, 0, 0); err != nil {
			return err
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		active := cfg.active(a.Getenv)
		type row struct {
			Name      string `json:"name"`
			URL       string `json:"url"`
			Workspace string `json:"workspace,omitempty"`
			Current   bool   `json:"current"`
			HasKey    bool   `json:"hasKey"`
		}
		var rows []row
		for _, n := range cfg.names() {
			cx, _ := cfg.lookup(n)
			rows = append(rows, row{n, cx.URL, cx.Workspace, n == active, cx.APIKey != ""})
		}
		if *c.json {
			return a.printValue(rows)
		}
		tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CURRENT\tNAME\tURL\tWORKSPACE\tAPI KEY")
		for _, r := range rows {
			cur, key := "", "none"
			if r.Current {
				cur = "*"
			}
			if r.HasKey {
				key = "set"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cur, r.Name, r.URL, r.Workspace, key)
		}
		return tw.Flush()
	case "rm", "remove":
		c := a.flags("context rm", "context rm <name>")
		pos, err := c.parse(rest, 1, 1)
		if err != nil {
			return err
		}
		name := pos[0]
		if name == CloudContext || name == LocalContext {
			return newError("invalid_argument", "the built-in "+name+" context cannot be removed")
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			return err
		}
		if _, ok := cfg.Contexts[name]; !ok {
			return newError("context_not_found", fmt.Sprintf("context %q does not exist", name))
		}
		delete(cfg.Contexts, name)
		if cfg.Current == name {
			cfg.Current = ""
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		if *c.json {
			return a.printValue(map[string]any{"removed": name})
		}
		fmt.Fprintf(a.Stdout, "removed context %s\n", name)
		return nil
	default:
		return usagef("usage: dock %s", use)
	}
}

// cmdLogin stores an API key read from stdin; a terminal reads it without echo.
func (a *App) cmdLogin(args []string) error {
	c := a.flags("login", "login [--context NAME]  (API key is read from stdin)")
	target := c.fs.String("context", "", "context to store the key in (default: current)")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	path, err := a.configPath()
	if err != nil {
		return err
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	name := *target
	switchToCloud := false
	if name == "" {
		// Logging in always means a control plane: from the local context it targets cloud.
		if name = cfg.active(a.Getenv); name == LocalContext {
			name, switchToCloud = CloudContext, true
		}
	}
	if name == LocalContext {
		return newError("invalid_argument", "the local context has no API key; use 'dock login --context cloud' or a context you added")
	}
	if _, ok := cfg.lookup(name); !ok {
		return newError("context_not_found", fmt.Sprintf("context %q does not exist", name))
	}
	var line string
	var rerr error
	if a.StdinIsTTY && a.ReadSecret != nil {
		fmt.Fprint(a.Stderr, "API key: ")
		line, rerr = a.ReadSecret()
		fmt.Fprintln(a.Stderr)
	} else {
		line, rerr = bufio.NewReader(a.Stdin).ReadString('\n')
	}
	key := strings.TrimSpace(line)
	if key == "" || strings.ContainsAny(key, " \t") {
		if rerr != nil && key == "" {
			return newError("invalid_argument", "no API key on stdin")
		}
		return newError("invalid_argument", "API key must be a single token")
	}
	cx := cfg.Contexts[name]
	cx.APIKey = key
	cfg.Contexts[name] = cx
	if switchToCloud {
		cfg.Current = name
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	if *c.json {
		return a.printValue(map[string]any{"context": name, "loggedIn": true})
	}
	fmt.Fprintf(a.Stdout, "stored API key for context %s\n", name)
	return nil
}
