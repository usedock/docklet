package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// isHostHelper reports a docker run of the host-block helper (host network namespace).
func isHostHelper(args []string) bool {
	i := slices.Index(args, "--network")
	return args[0] == "run" && i >= 0 && args[i+1] == "host"
}

// hostHelper is the fake of the helper container: it applies the script's mode to hostRules.
// The caller holds f.mu.
func (f *fakeDocker) hostHelper(args []string) (string, string, error) {
	c := slices.Index(args, "-c")
	if c < 0 || len(args) < c+6 || args[c+2] != "sh" {
		return "", "bad helper argv", errors.New("exit status 2")
	}
	mode, chain, bridge, rest := args[c+3], args[c+4], args[c+5], args[c+6:]
	switch mode {
	case "install":
		if f.hostErr != nil {
			return "", "cannot install the INPUT rule with iptables", f.hostErr
		}
		f.hostRules[chain] = append([]string{bridge}, rest...)
		return "", f.hostWarn, nil
	case "check":
		if f.hostCheckErr != nil {
			return "", "docker: cannot run the helper", f.hostCheckErr
		}
		if _, ok := f.hostRules[chain]; !ok {
			return gvHostBlockMissing + "\n", "", errors.New("exit status 3")
		}
	case "remove":
		if f.hostRmErr != nil {
			return "", "remove failed", f.hostRmErr
		}
		delete(f.hostRules, chain)
	}
	return "", "", nil
}

func (f *fakeDocker) rules() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.hostRules {
		out[k] = v
	}
	return out
}

// helperCalls returns the host-helper runs, in order, as "mode chain".
func (f *fakeDocker) helperCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if isHostHelper(c) {
			i := slices.Index(c, "-c")
			out = append(out, c[i+3]+" "+c[i+4])
		}
	}
	return out
}

func TestHostHelperArgsAreMinimal(t *testing.T) {
	argv := strings.Join(hostHelperArgs("img:1", "SCRIPT", "install", "DOCK-x", "dklx", "10.0.0.2"), " ")
	for _, want := range []string{"run --rm --runtime=runc --network host", "--cap-drop ALL", "--cap-add NET_ADMIN", "--cap-add NET_RAW", "--security-opt no-new-privileges", "--entrypoint sh img:1 -c SCRIPT sh install DOCK-x dklx 10.0.0.2"} {
		if !strings.Contains(argv, want) {
			t.Errorf("helper argv lacks %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "privileged") || strings.Contains(argv, "--pid") || strings.Contains(argv, " -v ") {
		t.Errorf("the helper must not be privileged or mount anything: %s", argv)
	}
}

func TestGVisorHostBlockLifecycle(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "hb", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	chain, bridge := gvChain(d.ID), gvBridge(d.ID)
	if len(bridge) > 15 {
		t.Fatalf("bridge %q is longer than an interface name may be", bridge)
	}
	rules := fd.rules()
	if got := rules[chain]; len(got) < 2 || got[0] != bridge {
		t.Fatalf("host block for %s: %v", chain, rules)
	}
	// The network is created with the bridge name the rule matches, and the rule exists before the container.
	var order []string
	for _, c := range fd.calls {
		switch {
		case c[0] == "network" && c[1] == "create":
			if !slices.Contains(c, "com.docker.network.bridge.name="+bridge) {
				t.Errorf("network create lacks the bridge name: %v", c)
			}
			order = append(order, "net")
		case isHostHelper(c):
			order = append(order, "block")
		case c[0] == "run":
			order = append(order, "run")
		}
	}
	if strings.Join(order, ",") != "net,block,run" {
		t.Fatalf("order %v: the block must be in place before the container starts", order)
	}
	// The dock's own resolvers are the only DNS exceptions.
	raw, _ := os.ReadFile(e.file(d.ID, "resolv.conf"))
	if want := gvNameservers(raw); !slices.Equal(rules[chain][1:], want) || len(want) == 0 {
		t.Fatalf("dns exceptions %v, resolv.conf has %v", rules[chain][1:], want)
	}

	if _, err := e.Stop(ctx, "hb", false); err != nil {
		t.Fatal(err)
	}
	if r := fd.rules(); len(r) != 0 {
		t.Fatalf("stop must remove the host block: %v", r)
	}
	before := len(fd.calls)
	if _, err := e.Resume(ctx, "hb"); err != nil {
		t.Fatal(err)
	}
	if r := fd.rules(); len(r[chain]) == 0 || r[chain][0] != bridge {
		t.Fatalf("resume must install the host block again: %v", r)
	}
	// Resume installs the block after the network exists and before the container is attached and started.
	var resumeOrder []string
	for _, c := range fd.calls[before:] {
		switch {
		case c[0] == "network" && c[1] == "create":
			resumeOrder = append(resumeOrder, "net")
		case isHostHelper(c):
			resumeOrder = append(resumeOrder, "block")
		case c[0] == "network" && c[1] == "connect":
			resumeOrder = append(resumeOrder, "connect")
		case c[0] == "start":
			resumeOrder = append(resumeOrder, "start")
		}
	}
	if strings.Join(resumeOrder, ",") != "net,block,connect,start" {
		t.Fatalf("resume order %v", resumeOrder)
	}
	if err := e.Remove(ctx, "hb"); err != nil {
		t.Fatal(err)
	}
	if r := fd.rules(); len(r) != 0 {
		t.Fatalf("rm must leave no rules: %v", r)
	}
}

func TestGVisorHostBlockFailsClosed(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.hostErr = errors.New("exit status 1")
	_, err := e.New(ctx, Spec{Name: "closed", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "host_block_failed" || !strings.Contains(err.Error(), gvHostBlockEnv) || !strings.Contains(err.Error(), "--network off") {
		t.Fatalf("want host_block_failed naming the opt-out, got %v", err)
	}
	if len(fd.containers) != 0 || len(fd.networks) != 0 || len(fd.rules()) != 0 || len(fd.callsWith("run")) != 0 {
		t.Fatalf("a dock that cannot be blocked must not start or leave anything: containers %v networks %v rules %v", fd.containers, fd.networks, fd.rules())
	}
	if docks, _ := e.List(ctx); len(docks) != 0 {
		t.Fatalf("no record may remain: %v", docks)
	}
}

func TestGVisorHostBlockOptOut(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	e.GVisor.AllowHost = true
	var logs strings.Builder
	e.Progress = &logs
	fd.hostErr = errors.New("would fail")
	fd.hostRmErr = errors.New("would fail")
	d, err := e.New(ctx, Spec{Name: "open", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "warning: "+gvHostBlockEnv) {
		t.Fatalf("the opt-out must warn: %q", logs.String())
	}
	if _, err := e.Stop(ctx, "open", false); err != nil {
		t.Fatalf("an unsupported removal must not block stop when the block is off: %v", err)
	}
	if err := e.Remove(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
}

func TestGVisorHostBlockShowsHelperWarnings(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	var logs strings.Builder
	e.Progress = &logs
	fd.hostWarn = "warning: no IPv6 rule (ip6tables)\n"
	if _, err := e.New(ctx, Spec{Name: "warn", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "warning: no IPv6 rule (ip6tables)"); n != 1 {
		t.Fatalf("want the warning logged once, got %d: %q", n, logs.String())
	}

	e2, _ := newGVisorEngine(t)
	var quiet strings.Builder
	e2.Progress = &quiet
	if _, err := e2.New(ctx, Spec{Name: "quiet", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet.String(), "no IPv6") {
		t.Fatalf("no warning expected: %q", quiet.String())
	}
}

func TestGVisorHostBlockNotUsedByNetworkOffDocks(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "off", Runtime: RuntimeGVisor, Network: "off", CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "off", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, "off"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if h := fd.helperCalls(); len(h) != 0 {
		t.Fatalf("a network-off dock has no bridge and needs no host block: %v", h)
	}
}

func TestGVisorHostBlockRemovedWhenBootFails(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	fd.agentDead = true
	e.BootTimeout = 600 * time.Millisecond
	if _, err := e.New(ctx, Spec{Name: "dead", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512}); err == nil {
		t.Fatal("want a boot error")
	}
	if r := fd.rules(); len(r) != 0 {
		t.Fatalf("a failed boot must remove the host block: %v", r)
	}
	// A failed resume (the agent never answers) removes it too.
	fd.agentDead = false
	d, err := e.New(ctx, Spec{Name: "res", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "res", false); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.exitOnRun = true
	fd.mu.Unlock()
	if _, err := e.Resume(ctx, "res"); err == nil {
		t.Fatal("want a resume error")
	}
	if r := fd.rules(); len(r) != 0 {
		t.Fatalf("a failed resume must remove the host block of %s: %v", d.ID, r)
	}
}

func TestGVisorHostBlockPerBranch(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	src, err := e.New(ctx, Spec{Name: "src", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := e.Branch(ctx, "src", 1, "br", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	r := fd.rules()
	if len(r) != 2 || len(r[gvChain(src.ID)]) == 0 || len(r[gvChain(bs[0].ID)]) == 0 {
		t.Fatalf("source and branch each need their own block: %v", r)
	}
	if err := e.Remove(ctx, "br"); err != nil {
		t.Fatal(err)
	}
	if r := fd.rules(); len(r) != 1 || len(r[gvChain(src.ID)]) == 0 {
		t.Fatalf("removing the branch must keep the source's block: %v", r)
	}
	if err := e.Remove(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	if r := fd.rules(); len(r) != 0 {
		t.Fatalf("leftover rules: %v", r)
	}
}

// A network an older dock left (Docker's br-<id> bridge, the stopped container still attached)
// is replaced on resume, so the rule's -i dkl<id> matches the bridge the dock really uses.
func TestGVisorResumeReplacesAnOldStyleNetwork(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "old", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stop(ctx, "old", false); err != nil {
		t.Fatal(err)
	}
	netName, ctr := gvNetwork(d.ID), gvContainer(d.ID)
	fd.mu.Lock()
	fd.networks[netName], fd.bridges[netName], fd.attached[ctr] = true, "", true
	fd.mu.Unlock()
	if _, err := e.Resume(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	bridge, attached := fd.bridges[netName], fd.attached[ctr]
	fd.mu.Unlock()
	if bridge != gvBridge(d.ID) || !attached {
		t.Fatalf("resume kept the old network: bridge %q attached %v", bridge, attached)
	}
	if r := fd.rules(); len(r[gvChain(d.ID)]) == 0 || r[gvChain(d.ID)][0] != bridge {
		t.Fatalf("the rule does not match the dock's bridge %q: %v", bridge, r)
	}

	// A network that already has the right bridge is kept as it is.
	if _, err := e.Stop(ctx, "old", false); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	fd.networks[netName], fd.bridges[netName], fd.attached[ctr] = true, gvBridge(d.ID), true
	before := len(fd.calls)
	fd.mu.Unlock()
	if _, err := e.Resume(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fd.calls[before:] {
		if c[0] == "network" && (c[1] == "rm" || c[1] == "disconnect") {
			t.Fatalf("a current network must not be replaced: %v", c)
		}
	}
}

func TestGvNameservers(t *testing.T) {
	got := gvNameservers([]byte("# c\nnameserver 192.168.1.1\nnameserver 8.8.8.8\nsearch x\n"))
	if !slices.Equal(got, []string{"192.168.1.1", "8.8.8.8"}) {
		t.Fatalf("%v", got)
	}
}

// The helper script really runs under sh against a stateful fake iptables: install is idempotent,
// falls back to iptables-legacy when the first backend is unusable, and remove leaves nothing.
func TestHostBlockScriptWithFakeIptables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	bin, state := t.TempDir(), t.TempDir()
	fake := `#!/bin/sh
n=$(basename "$0")
[ "$n" = "$BROKEN" ] && exit 1
f="$STATE/$n"; touch "$f"
[ "$1" = -w ] && shift
case "$1" in
 -S) [ -z "${2:-}" ] || grep -qxF "chain $2" "$f"; exit $?;;
 -N) grep -qxF "chain $2" "$f" && exit 1; echo "chain $2" >> "$f";;
 -F) grep -qxF "chain $2" "$f" || exit 1; grep -v "^rule $2 " "$f" > "$f.n"; mv "$f.n" "$f";;
 -X) grep -qxF "chain $2" "$f" || exit 1; grep -vxF "chain $2" "$f" > "$f.n"; mv "$f.n" "$f";;
 -A) c=$2; shift 2; echo "rule $c $*" >> "$f";;
 -I) c=$2; shift 3; echo "rule $c $*" >> "$f";;
 -C) c=$2; shift 2; grep -qxF "rule $c $*" "$f";;
 -D) c=$2; shift 2; l="rule $c $*"; grep -qxF "$l" "$f" || exit 1; awk -v l="$l" '!d && $0==l {d=1; next} {print}' "$f" > "$f.n"; mv "$f.n" "$f";;
esac
`
	for _, n := range []string{"iptables", "iptables-legacy", "ip6tables", "ip6tables-legacy"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	command := func(broken string, args ...string) *exec.Cmd {
		cmd := exec.Command(sh, append([]string{"-c", gvHostBlockScript, "sh"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STATE="+state, "BROKEN="+broken)
		return cmd
	}
	run := func(broken string, args ...string) {
		t.Helper()
		if out, err := command(broken, args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(state, n)); return string(b) }
	jump := "rule INPUT -i dklb -j DOCK-b\n"

	run("", "install", "DOCK-b", "dklb", "192.168.1.1")
	run("", "install", "DOCK-b", "dklb", "192.168.1.1")
	got := read("iptables")
	if strings.Count(got, jump) != 1 {
		t.Fatalf("install twice must leave one jump:\n%s", got)
	}
	for _, want := range []string{
		"rule DOCK-b -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n",
		"rule DOCK-b -p udp --dport 53 -d 192.168.1.1 -j ACCEPT\n",
		"rule DOCK-b -p tcp --dport 53 -d 192.168.1.1 -j ACCEPT\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want %q once:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "rule DOCK-b -j DROP\n") || strings.Index(got, "ESTABLISHED") > strings.Index(got, "-j DROP") {
		t.Errorf("the DROP must come last, after conntrack:\n%s", got)
	}
	if v6 := read("ip6tables"); !strings.Contains(v6, jump) || strings.Contains(v6, "dport 53") {
		t.Errorf("IPv6 gets the block without v4 resolvers:\n%s", v6)
	}
	if read("iptables-legacy") != "" {
		t.Error("the legacy backend must not be touched when the first one works")
	}
	run("", "remove", "DOCK-b", "dklb")
	for _, n := range []string{"iptables", "ip6tables"} {
		if s := read(n); strings.Contains(s, "rule") || strings.Contains(s, "chain") {
			t.Errorf("remove left rules in %s:\n%s", n, s)
		}
	}
	run("", "remove", "DOCK-b", "dklb") // idempotent

	// Check: present, then jump missing, then chain missing, then chain emptied, then on the legacy backend.
	checkOut := func(broken string) (string, error) {
		out, err := command(broken, "check", "DOCK-b", "dklb").Output()
		return string(out), err
	}
	run("", "install", "DOCK-b", "dklb", "192.168.1.1")
	if out, err := checkOut(""); err != nil {
		t.Fatalf("check with the block present: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(state, "iptables"), []byte(strings.Replace(read("iptables"), jump, "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := checkOut(""); err == nil || !strings.Contains(out, gvHostBlockMissing) {
		t.Fatalf("check with the jump missing: %v %q", err, out)
	}
	if err := os.WriteFile(filepath.Join(state, "iptables"), []byte(jump), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := checkOut(""); err == nil || !strings.Contains(out, gvHostBlockMissing) {
		t.Fatalf("check with the chain missing: %v %q", err, out)
	}
	// The chain is emptied (iptables -F DOCK-b): the jump leads nowhere, so the dock is unblocked.
	if err := os.WriteFile(filepath.Join(state, "iptables"), []byte(jump+"chain DOCK-b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := checkOut(""); err == nil || !strings.Contains(out, gvHostBlockMissing) {
		t.Fatalf("check with the chain emptied: %v %q", err, out)
	}
	run("", "remove", "DOCK-b", "dklb")
	run("iptables", "install", "DOCK-b", "dklb")
	if out, err := checkOut("iptables"); err != nil {
		t.Fatalf("check on the legacy backend: %v %s", err, out)
	}
	run("iptables", "remove", "DOCK-b", "dklb")

	// The nft backend is unusable here (a kernel without nf_tables): the legacy one is used.
	run("iptables", "install", "DOCK-b", "dklb")
	if !strings.Contains(read("iptables-legacy"), jump) {
		t.Fatalf("fallback to iptables-legacy: %q", read("iptables-legacy"))
	}
	run("iptables", "remove", "DOCK-b", "dklb")
	if s := read("iptables-legacy"); strings.Contains(s, "rule") || strings.Contains(s, "chain") {
		t.Fatalf("remove left rules: %q", s)
	}
	// No usable backend at all is an error, not a silent success.
	if err := command("iptables", "install", "DOCK-b", "dklb").Run(); err != nil {
		t.Fatalf("one working backend is enough: %v", err)
	}
	for _, n := range []string{"iptables", "iptables-legacy"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := command("", "install", "DOCK-b", "dklb").Run(); err == nil {
		t.Fatal("install must fail when no iptables backend works")
	}
}

func TestCheckHostBlock(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	d, err := e.New(ctx, Spec{Name: "cb", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	g := e.gvisor()
	if ok, err := g.checkHostBlock(ctx, d.ID); !ok || err != nil {
		t.Fatalf("present: %v %v", ok, err)
	}
	fd.mu.Lock()
	delete(fd.hostRules, gvChain(d.ID))
	fd.mu.Unlock()
	if ok, err := g.checkHostBlock(ctx, d.ID); ok || err != nil {
		t.Fatalf("missing must be (false, nil): %v %v", ok, err)
	}
	// The current image is gone (a newer dock has a new image tag): the check never builds and
	// runs the helper with the image the dock's container runs.
	_, tag, err := gvBuildContext()
	if err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	delete(fd.images, tag)
	builds := fd.builds
	fd.mu.Unlock()
	if ok, err := g.checkHostBlock(ctx, d.ID); ok || err != nil {
		t.Fatalf("missing without the current image: %v %v", ok, err)
	}
	fd.mu.Lock()
	rebuilt := fd.builds != builds
	fd.mu.Unlock()
	if rebuilt {
		t.Fatal("the check must not build the image")
	}
	fd.mu.Lock()
	last := fd.calls[len(fd.calls)-1]
	fd.mu.Unlock()
	if !slices.Contains(last, "sha256:fakecontainerimage") || !slices.Contains(last, "check") {
		t.Fatalf("the check must run with the container's image: %v", last)
	}
	fd.mu.Lock()
	fd.hostCheckErr = errors.New("exit status 125")
	fd.mu.Unlock()
	if ok, err := g.checkHostBlock(ctx, d.ID); ok || err == nil {
		t.Fatalf("a helper that cannot run is an error, not a missing rule: %v %v", ok, err)
	}
}

func TestDoctorReportsMissingHostBlock(t *testing.T) {
	ctx := context.Background()
	e, fd := newGVisorEngine(t)
	e.Driver = &QEMU{GOOS: "linux", GOARCH: "amd64", Getenv: env(nil), system: "/q/qemu", img: "/q/qemu-img", KVM: func() error { return nil }}
	fd.runtimes = `{"runsc":{}}`
	rows := func() map[string]Check {
		m := map[string]Check{}
		for _, c := range e.Doctor(ctx) {
			if strings.HasPrefix(c.Name, "host block") {
				m[c.Name] = c
			}
		}
		return m
	}
	if r := rows(); len(r) != 0 {
		t.Fatalf("no gvisor docks, no rows: %v", r)
	}
	d, err := e.New(ctx, Spec{Name: "hbd", Runtime: RuntimeGVisor, CPUs: 1, MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.New(ctx, Spec{Name: "hbo", Runtime: RuntimeGVisor, Network: "off", CPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}
	if r := rows(); len(r) != 1 || !r["host block hbd"].OK {
		t.Fatalf("a healthy network-on dock passes, network off has no row: %v", r)
	}
	fd.mu.Lock()
	delete(fd.hostRules, gvChain(d.ID))
	fd.mu.Unlock()
	if c := rows()["host block hbd"]; c.OK || c.Optional || c.Fix != "dock stop hbd && dock resume hbd" {
		t.Fatalf("missing rule row %+v", c)
	}
	e.GVisor.AllowHost = true
	if r := rows(); len(r) != 0 {
		t.Fatalf("the block is off, no rows: %v", r)
	}
	e.GVisor.AllowHost = false
	fd.mu.Lock()
	fd.hostCheckErr = errors.New("exit status 125")
	fd.mu.Unlock()
	if c := rows()["host block hbd"]; c.OK || !c.Optional || c.Fix != "" {
		t.Fatalf("an unverifiable rule is informational: %+v", c)
	}
	fd.mu.Lock()
	fd.hostCheckErr = nil
	fd.mu.Unlock()
	if _, err := e.Stop(ctx, "hbd", false); err != nil {
		t.Fatal(err)
	}
	if r := rows(); len(r) != 0 {
		t.Fatalf("a stopped dock is not checked: %v", r)
	}
	if _, err := e.Resume(ctx, "hbd"); err != nil {
		t.Fatal(err)
	}
	if c := rows()["host block hbd"]; !c.OK {
		t.Fatalf("resume restores the rule: %+v", c)
	}
}
