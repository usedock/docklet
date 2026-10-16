package local

import (
	"context"
	"os"
	"strings"
)

// A network-on gvisor dock reaches the host through its bridge gateway: traffic to any address
// the host owns (the gateway, docker0, the LAN IP) is delivered to the host's INPUT chain, which
// Docker does not filter. The block is one INPUT rule per dock bridge, installed by Docker itself
// (a short-lived helper container with NET_ADMIN in the host network namespace), so it lives
// outside the sandbox and root in the dock cannot remove it, and it needs no sudo from the user.
// DOCKER-USER is not used: it only sees forwarded traffic, never traffic to the host itself.

// gvHostBlockEnv turns the block off (the dock then reaches host services, as before).
const gvHostBlockEnv = "DOCK_GVISOR_ALLOW_HOST"

// gvHostBlockScript runs in the helper as: sh -c script sh <install|remove> <chain> <bridge> [dns...].
// Install is idempotent (it rewrites the chain and adds the jump once). IPv4 is required; IPv6 is
// best effort. Replies to connections the host opened (the published agent port, via docker-proxy
// or DNAT) are conntrack ESTABLISHED and pass; anything the dock starts is dropped, except DNS
// to the resolvers in the dock's resolv.conf.
const gvHostBlockScript = `set -u
mode=$1; chain=$2; br=$3; shift 3
install() {
  t=$1; shift
  "$t" -w -N "$chain" 2>/dev/null || "$t" -w -F "$chain" || return 1
  "$t" -w -A "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT || return 1
  for ns in "$@"; do
    "$t" -w -A "$chain" -p udp --dport 53 -d "$ns" -j ACCEPT || return 1
    "$t" -w -A "$chain" -p tcp --dport 53 -d "$ns" -j ACCEPT || return 1
  done
  "$t" -w -A "$chain" -j DROP || return 1
  "$t" -w -C INPUT -i "$br" -j "$chain" 2>/dev/null || "$t" -w -I INPUT 1 -i "$br" -j "$chain"
}
if [ "$mode" = install ]; then
  ok=
  for t in iptables iptables-legacy; do
    if "$t" -w -S >/dev/null 2>&1; then install "$t" "$@" && ok=1; break; fi
  done
  [ -n "$ok" ] || { echo "cannot install the INPUT rule with iptables" >&2; exit 1; }
  for t in ip6tables ip6tables-legacy; do
    if "$t" -w -S >/dev/null 2>&1; then install "$t" || echo "warning: no IPv6 rule ($t)" >&2; break; fi
  done
  exit 0
fi
for t in iptables iptables-legacy ip6tables ip6tables-legacy; do
  while "$t" -w -D INPUT -i "$br" -j "$chain" 2>/dev/null; do :; done
  "$t" -w -F "$chain" 2>/dev/null
  "$t" -w -X "$chain" 2>/dev/null
done
exit 0
`

// gvDockHex is the part of a dock id the host names derive from (a Linux interface name holds 15 bytes).
func gvDockHex(id string) string {
	h := strings.TrimPrefix(id, "dk_")
	if len(h) > 10 {
		h = h[:10]
	}
	return h
}

// gvBridge is the host bridge interface of a dock network; it is set at network creation so the
// rule does not depend on Docker's generated br-<network id> name.
func gvBridge(id string) string { return "dkl" + gvDockHex(id) }

// gvChain is the iptables chain holding the dock's rules.
func gvChain(id string) string { return "DOCK-" + gvDockHex(id) }

// gvNameservers lists the nameserver addresses of a resolv.conf.
func gvNameservers(resolv []byte) []string {
	var out []string
	for _, line := range strings.Split(string(resolv), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

// hostHelperArgs is the docker run for a one-shot helper in the host network namespace with only
// the capabilities iptables needs (not --privileged). It runs under runc: the sandbox runtime
// cannot give it the host's network.
func hostHelperArgs(image, script string, args ...string) []string {
	a := []string{"run", "--rm", "--runtime=runc", "--network", "host", "--user", "0:0",
		"--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
		"--security-opt", "no-new-privileges", "--entrypoint", "sh", image, "-c", script, "sh"}
	return append(a, args...)
}

// installHostBlock blocks the dock's bridge from the host's own addresses. It fails closed:
// without the rule the dock would reach host services. On success it returns the helper's
// "warning:" lines (such as a missing IPv6 rule) for the caller to show.
func (g *GVisor) installHostBlock(ctx context.Context, id string, nameservers []string) ([]string, error) {
	if g.AllowHost {
		return nil, nil
	}
	image, err := g.EnsureImage(ctx, nil)
	if err != nil {
		return nil, err
	}
	args := hostHelperArgs(image, gvHostBlockScript, append([]string{"install", gvChain(id), gvBridge(id)}, nameservers...)...)
	_, stderr, err := g.docker(ctx, nil, args...)
	if err != nil {
		low := strings.ToLower(stderr)
		if strings.Contains(low, "cannot connect to the docker daemon") || strings.Contains(low, "permission denied while trying to connect") {
			return nil, dockerFailure("boot_failed", "install the host block", err, stderr)
		}
		return nil, errf("host_block_failed", "cannot block the dock's network from this machine's services: %v: %s. Docker must allow a short-lived helper container with NET_ADMIN in the host network. To run the dock anyway, with access to your machine's network services, set %s=1; or use --network off", err, strings.TrimSpace(lastLines(stderr, 3)), gvHostBlockEnv)
	}
	var warns []string
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "warning:") {
			warns = append(warns, l)
		}
	}
	return warns, nil
}

// removeHostBlock deletes the dock's rules; rules that are already gone are fine.
func (g *GVisor) removeHostBlock(ctx context.Context, id string) error {
	image, err := g.EnsureImage(ctx, nil)
	if err != nil {
		return err
	}
	args := hostHelperArgs(image, gvHostBlockScript, "remove", gvChain(id), gvBridge(id))
	if _, stderr, err := g.docker(ctx, nil, args...); err != nil && !g.AllowHost {
		return dockerFailure("stop_failed", "remove the host block "+gvChain(id), err, stderr)
	}
	return nil
}

// gvHostBlock installs the dock's host block from its recorded resolv.conf.
func (e *Engine) gvHostBlock(ctx context.Context, rec Record) error {
	g := e.gvisor()
	if g.AllowHost {
		e.logf("warning: %s is set, so this dock can reach services on this machine (any address the host owns)", gvHostBlockEnv)
		return nil
	}
	raw, _ := os.ReadFile(e.file(rec.ID, "resolv.conf"))
	warns, err := g.installHostBlock(ctx, rec.ID, gvNameservers(raw))
	for _, w := range warns {
		e.logf("%s", w)
	}
	return err
}
