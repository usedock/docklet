package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
)

type dock struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Image       string `json:"image"`
	Runtime     string `json:"runtime"`
	Environment string `json:"environment"`
	CPUs        int    `json:"cpus"`
	MemoryMiB   int    `json:"memoryMiB"`
	DiskGiB     int    `json:"diskGiB"`
}

func (d dock) label() string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// specFlags are the size flags shared by new and branch.
type specFlags struct {
	cpus                     int
	memory, disk, image, env string
}

func registerSpecFlags(c *cmdFlags) *specFlags {
	s := &specFlags{}
	c.fs.IntVar(&s.cpus, "cpus", 0, "number of CPUs")
	c.fs.StringVar(&s.memory, "memory", "", "memory, e.g. 8gb or 8192mb (plain number is MiB)")
	c.fs.StringVar(&s.disk, "disk", "", "disk, e.g. 40gb (plain number is GiB)")
	c.fs.StringVar(&s.image, "image", "", "image, e.g. ubuntu-24.04")
	c.fs.StringVar(&s.env, "env", "", "environment name")
	return s
}

// overlay applies the flags that were set onto spec.
func (s *specFlags) overlay(c *cmdFlags, spec *dockSpec) error {
	if c.isSet("cpus") {
		if s.cpus < 1 {
			return usagef("--cpus must be a positive integer")
		}
		spec.CPUs = s.cpus
	}
	if c.isSet("memory") {
		v, err := ParseMemoryMiB(s.memory)
		if err != nil {
			return usagef("--memory: %v", err)
		}
		spec.MemoryMiB = v
	}
	if c.isSet("disk") {
		v, err := ParseDiskGiB(s.disk)
		if err != nil {
			return usagef("--disk: %v", err)
		}
		spec.DiskGiB = v
	}
	if c.isSet("image") {
		spec.Image = s.image
	}
	if c.isSet("env") {
		spec.Environment = s.env
	}
	return nil
}

// resolveWorkspace picks the given workspace or the first one of the first organization.
func (a *App) resolveWorkspace(ctx context.Context, cl *Client, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	var orgs []struct {
		ID string `json:"id"`
	}
	raw, err := cl.Do(ctx, "GET", "/organizations", nil, nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &orgs); err != nil || len(orgs) == 0 {
		return "", newError("no_workspace", "no organization found; pass --workspace <id>")
	}
	var wss []struct {
		ID string `json:"id"`
	}
	raw, err = cl.Do(ctx, "GET", "/organizations/"+orgs[0].ID+"/workspaces", nil, nil)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &wss); err != nil || len(wss) == 0 {
		return "", newError("no_workspace", "no workspace found; pass --workspace <id>")
	}
	return wss[0].ID, nil
}

// resolveDock turns a name or id into a dock id.
func (a *App) resolveDock(ctx context.Context, cl *Client, ref string) (string, error) {
	if uuidRE.MatchString(ref) {
		return ref, nil
	}
	raw, err := cl.Do(ctx, "GET", "/docks?all=true", nil, nil)
	if err != nil {
		return "", err
	}
	var docks []dock
	if err := json.Unmarshal(raw, &docks); err != nil {
		return "", newError("bad_response", "unexpected dock list")
	}
	var match []dock
	for _, d := range docks {
		if d.Name == ref {
			match = append(match, d)
		}
	}
	switch len(match) {
	case 0:
		return "", newError("dock_not_found", fmt.Sprintf("no dock named %q", ref))
	case 1:
		return match[0].ID, nil
	default:
		return "", newError("ambiguous_dock", fmt.Sprintf("%d docks are named %q; use the id", len(match), ref))
	}
}

func (a *App) cmdNew(ctx context.Context, args []string) error {
	c := a.flags("new", "new [name] [--cpus N] [--memory 8gb] [--disk 40gb] [--image X] [--env E] [--workspace ID]")
	sf := registerSpecFlags(c)
	ws := c.fs.String("workspace", "", "workspace id (default: first workspace)")
	pos, err := c.parse(args, 0, 1)
	if err != nil {
		return err
	}
	dir := a.WorkDir
	if dir == "" {
		dir = "."
	}
	spec, _, err := readDockFile(dir)
	if err != nil {
		return err
	}
	if err := sf.overlay(c, &spec); err != nil {
		return err
	}
	if len(pos) == 1 {
		spec.Name = pos[0]
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	wsID, err := a.resolveWorkspace(ctx, cl, *ws)
	if err != nil {
		return err
	}
	body := spec.body()
	body["workspaceId"] = wsID
	raw, err := cl.Do(ctx, "POST", "/docks", nil, body)
	if err != nil {
		return err
	}
	if *c.json {
		return a.printJSON(raw)
	}
	var d dock
	_ = json.Unmarshal(raw, &d)
	fmt.Fprintf(a.Stdout, "created %s (%s) state=%s\n", d.label(), d.ID, d.State)
	return nil
}

func (a *App) cmdLs(ctx context.Context, args []string) error {
	c := a.flags("ls", "ls [--all] [--filter rspte]")
	all := c.fs.Bool("all", false, "include stopped docks")
	filter := c.fs.String("filter", "", "state group letters (rspte)")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	path := "/docks"
	switch {
	case *all:
		path += "?all=true"
	case *filter != "":
		path += "?filter=" + *filter
	}
	raw, err := cl.Do(ctx, "GET", path, nil, nil)
	if err != nil {
		return err
	}
	if *c.json {
		return a.printJSON(raw)
	}
	var docks []dock
	if err := json.Unmarshal(raw, &docks); err != nil {
		return newError("bad_response", "unexpected dock list")
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tCPUS\tMEMORY\tDISK\tIMAGE")
	for _, d := range docks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%dgb\t%s\n", d.ID, d.Name, d.State, d.CPUs, formatMiB(d.MemoryMiB), d.DiskGiB, d.Image)
	}
	return tw.Flush()
}

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellJoin builds one bash command line; a single argument is passed through as a script.
func shellJoin(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	q := make([]string, len(args))
	for i, s := range args {
		if shellSafeRE.MatchString(s) {
			q[i] = s
		} else {
			q[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

func (a *App) cmdExec(ctx context.Context, args []string) error {
	// Everything after the first "--" is the command.
	var cmdArgs []string
	for i, s := range args {
		if s == "--" {
			args, cmdArgs = args[:i], args[i+1:]
			break
		}
	}
	c := a.flags("exec", "exec <dock> [--timeout S] [--cwd DIR] -- <cmd...>")
	timeout := c.fs.Int("timeout", 0, "seconds before the command is killed (1-600)")
	cwd := c.fs.String("cwd", "", "absolute working directory in the dock")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if len(cmdArgs) == 0 {
		return usagef("usage: dock %s", c.use)
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	body := map[string]any{"command": shellJoin(cmdArgs)}
	if *timeout > 0 {
		body["timeoutSeconds"] = *timeout
	}
	if *cwd != "" {
		body["cwd"] = *cwd
	}
	raw, err := cl.Do(ctx, "POST", "/docks/"+id+"/exec", nil, body)
	if err != nil {
		return err
	}
	var res struct {
		ExitCode  int    `json:"exitCode"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return newError("bad_response", "unexpected exec result")
	}
	if *c.json {
		if err := a.printJSON(raw); err != nil {
			return err
		}
	} else {
		fmt.Fprint(a.Stdout, res.Stdout)
		fmt.Fprint(a.Stderr, res.Stderr)
		if res.Truncated {
			fmt.Fprintln(a.Stderr, "warning: output was truncated to 1 MiB per stream")
		}
	}
	if res.ExitCode != 0 {
		return &exitError{code: res.ExitCode}
	}
	return nil
}

func (a *App) cmdSSH(ctx context.Context, args []string) error {
	c := a.flags("ssh", "ssh <dock> [--print]")
	printOnly := c.fs.Bool("print", false, "print the ssh command instead of running it")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	raw, err := cl.Do(ctx, "GET", "/config", nil, nil)
	if err != nil {
		return err
	}
	var cfg struct {
		SSH *struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"ssh"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return newError("bad_response", "unexpected config")
	}
	if cfg.SSH == nil {
		return newError("ssh_unavailable", "this control plane has no SSH gateway")
	}
	argv := []string{"ssh", "-p", strconv.Itoa(cfg.SSH.Port), id + "@" + cfg.SSH.Host}
	if *c.json {
		return a.printValue(map[string]any{"command": argv, "host": cfg.SSH.Host, "port": cfg.SSH.Port, "user": id})
	}
	if *printOnly {
		fmt.Fprintln(a.Stdout, strings.Join(argv, " "))
		return nil
	}
	run := a.RunSSH
	if run == nil {
		run = func(ctx context.Context, argv []string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, a.Stdout, a.Stderr
			return cmd.Run()
		}
	}
	if err := run(ctx, argv); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitError{code: ee.ExitCode()}
		}
		return newError("ssh_failed", fmt.Sprintf("%v (command: %s)", err, strings.Join(argv, " ")))
	}
	return nil
}

func (a *App) cmdPort(ctx context.Context, args []string) error {
	c := a.flags("port", "port <dock> <port> [--rm]")
	rm := c.fs.Bool("rm", false, "stop exposing the port")
	pos, err := c.parse(args, 2, 2)
	if err != nil {
		return err
	}
	port, perr := strconv.Atoi(pos[1])
	if perr != nil || port < 1 || port > 65535 {
		return usagef("port must be 1-65535")
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	if *rm {
		raw, err := cl.Do(ctx, "DELETE", "/docks/"+id+"/ports/"+strconv.Itoa(port), nil, nil)
		if err != nil {
			return err
		}
		if *c.json {
			return a.printJSON(raw)
		}
		fmt.Fprintf(a.Stdout, "port %d is no longer exposed\n", port)
		return nil
	}
	raw, err := cl.Do(ctx, "POST", "/docks/"+id+"/ports", nil, map[string]any{"port": port})
	if err != nil {
		return err
	}
	if *c.json {
		return a.printJSON(raw)
	}
	var p struct {
		URL *string `json:"url"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.URL == nil {
		fmt.Fprintf(a.Stdout, "port %d registered; hosted ports are off on this deployment, so it has no public URL\n", port)
		return nil
	}
	fmt.Fprintln(a.Stdout, *p.URL)
	return nil
}

// simpleAction runs stop or resume against one dock.
func (a *App) simpleAction(ctx context.Context, c *cmdFlags, args []string, action, past string, body map[string]any) error {
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, args[0])
	if err != nil {
		return err
	}
	raw, err := cl.Do(ctx, "POST", "/docks/"+id+"/"+action, nil, body)
	if err != nil {
		return err
	}
	if *c.json {
		return a.printJSON(raw)
	}
	var r struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(raw, &r)
	fmt.Fprintf(a.Stdout, "%s %s state=%s\n", past, args[0], r.State)
	return nil
}

func (a *App) cmdStop(ctx context.Context, args []string) error {
	c := a.flags("stop", "stop <dock> [--force]")
	force := c.fs.Bool("force", false, "stop even if the dock is busy")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if *force {
		body["force"] = true
	}
	return a.simpleAction(ctx, c, pos, "stop", "stopped", body)
}

func (a *App) cmdResume(ctx context.Context, args []string) error {
	c := a.flags("resume", "resume <dock>")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	return a.simpleAction(ctx, c, pos, "resume", "resumed", map[string]any{})
}

func (a *App) cmdBranch(ctx context.Context, args []string) error {
	c := a.flags("branch", "branch <dock> [--count N] [--name NAME] [--cpus N] [--memory 8gb] [--disk 40gb] [--env E]")
	sf := registerSpecFlags(c)
	count := c.fs.Int("count", 1, "number of branches to create")
	name := c.fs.String("name", "", "name of the new dock (only with --count 1)")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if *count < 1 || *count > 50 {
		return usagef("--count must be 1-50")
	}
	if *name != "" && *count > 1 {
		return usagef("--name needs --count 1 (names are unique)")
	}
	var spec dockSpec
	if err := sf.overlay(c, &spec); err != nil {
		return err
	}
	spec.Name = *name
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	var out []json.RawMessage
	var failure error
	for i := 0; i < *count; i++ {
		raw, err := cl.Do(ctx, "POST", "/docks/"+id+"/branch", nil, spec.body())
		if err != nil {
			failure = err
			break
		}
		out = append(out, raw)
		if !*c.json {
			var d dock
			_ = json.Unmarshal(raw, &d)
			fmt.Fprintf(a.Stdout, "branched %s -> %s (%s)\n", pos[0], d.label(), d.ID)
		}
	}
	if *c.json && len(out) > 0 {
		var raw json.RawMessage
		if *count == 1 {
			raw = out[0]
		} else {
			raw, _ = json.Marshal(out)
		}
		if err := a.printJSON(raw); err != nil {
			return err
		}
	}
	return failure
}

func (a *App) cmdRm(ctx context.Context, args []string) error {
	c := a.flags("rm", "rm <dock>")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	cl, err := a.client()
	if err != nil {
		return err
	}
	id, err := a.resolveDock(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	raw, err := cl.Do(ctx, "DELETE", "/docks/"+id, map[string]string{"x-confirm-delete": id}, nil)
	if err != nil {
		return err
	}
	if *c.json {
		return a.printJSON(raw)
	}
	fmt.Fprintf(a.Stdout, "removed %s\n", pos[0])
	return nil
}
