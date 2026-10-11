package host

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	apiv1 "github.com/usedock/docklet/api/v1"
	"github.com/usedock/docklet/internal/disk"
	"github.com/usedock/docklet/internal/egress"
	"github.com/usedock/docklet/internal/guestagent"
)

const fastAgentPort = guestagent.Port

// FastAgentProgramText exposes the guest-agent source so the OCI image
// build bakes the identical protocol (ping/exec/read/rekey) as a
// supervised sidecar instead of a systemd unit.
func FastAgentProgramText() string { return fastAgentProgram }

const fastAgentProgram = `#!/usr/bin/env python3
import base64
import fcntl
import json
import os
import pty
import pwd
import signal
import socket
import struct
import subprocess
import termios
import threading
import time

CONFIG = json.load(open('/etc/dock-fast-agent.json', encoding='utf-8'))
TOKEN = CONFIG['token']
WORKSPACE = os.path.realpath(CONFIG['workspace'])
ROOTS = [os.path.realpath(p) for p in CONFIG['roots']]

# /run is tmpfs on VM guests, so roots baked into the disk vanish at boot.
for _root in ROOTS:
    os.makedirs(_root, exist_ok=True)
    if os.geteuid() == 0:
        _owner = pwd.getpwnam('user')
        os.chown(_root, _owner.pw_uid, _owner.pw_gid)


def allowed(path):
    resolved = os.path.realpath(path)
    return any(resolved == root or resolved.startswith(root + os.sep) for root in ROOTS)


def response(conn, payload):
    conn.sendall((json.dumps(payload, separators=(',', ':')) + '\n').encode())


def drain(stream, limit, sink):
    # Keeps the first limit bytes and discards the rest so output never grows unbounded.
    while True:
        chunk = stream.read1(65536)
        if not chunk:
            break
        room = limit - len(sink['data'])
        if room > 0:
            sink['data'] += chunk[:room]
        if len(chunk) > room:
            sink['cut'] = True


def run_script(request):
    timeout = max(1, min(int(request.get('timeoutSeconds', 300)), 3600))
    limit = max(1, min(int(request.get('outputLimit', 1048576)), 8388608))
    argv = ['/bin/bash', '-lc', request.get('script', '')]
    # Root guests (microVMs) drop to the dock user; gvisor docks already run as it.
    if os.geteuid() == 0:
        argv = ['/usr/sbin/runuser', '-u', 'user', '--'] + argv
    proc = subprocess.Popen(
        argv,
        cwd=WORKSPACE,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env={**os.environ, **request.get('env', {}), 'HOME': '/home/user'},
        start_new_session=True,
    )
    out = {'data': bytearray(), 'cut': False}
    err = {'data': bytearray(), 'cut': False}
    readers = [
        threading.Thread(target=drain, args=(proc.stdout, limit, out), daemon=True),
        threading.Thread(target=drain, args=(proc.stderr, limit, err), daemon=True),
    ]
    for reader in readers:
        reader.start()
    timed_out = False
    try:
        code = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        proc.wait()
        code = 124
    # Background children may hold the pipes open; never wait on them for long.
    for reader in readers:
        reader.join(2)
    return {
        'ok': True,
        'exitCode': code,
        'stdout': bytes(out['data']).decode('utf-8', 'replace'),
        'stderr': bytes(err['data']).decode('utf-8', 'replace'),
        'truncated': out['cut'] or err['cut'],
        'timedOut': timed_out,
    }


# Terminal frames after the pty handshake: kind byte, big-endian length, payload.
FRAME_DATA = 0
FRAME_RESIZE = 1
FRAME_EXIT = 2
MAX_FRAME = 65536


def read_exact(reader, size):
    data = reader.read(size)
    if data is None or len(data) < size:
        raise EOFError
    return data


def send_frame(conn, kind, payload):
    conn.sendall(struct.pack('>BI', kind, len(payload)) + payload)


def set_winsize(fd, cols, rows):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', max(1, min(rows, 1000)), max(1, min(cols, 1000)), 0, 0))


def session_pids(sid):
    pids = []
    for entry in os.listdir('/proc'):
        if not entry.isdigit():
            continue
        try:
            with open('/proc/' + entry + '/stat', encoding='utf-8') as stat_file:
                stat = stat_file.read()
            if int(stat[stat.rfind(')') + 2:].split()[3]) == sid:
                pids.append(int(entry))
        except (OSError, ValueError, IndexError):
            pass
    return pids


def hang_up(sid):
    # Like an ssh disconnect: SIGHUP the shell's session, then kill what ignores it.
    for sig in (signal.SIGHUP, signal.SIGKILL):
        pids = session_pids(sid)
        if not pids:
            return
        for pid in pids:
            try:
                os.kill(pid, sig)
            except OSError:
                pass
        for _ in range(20):
            if not session_pids(sid):
                return
            time.sleep(0.1)


def run_pty(conn, reader, request):
    cols = int(request.get('cols', 80))
    rows = int(request.get('rows', 24))
    env = {
        'HOME': '/home/user',
        'USER': 'user',
        'LOGNAME': 'user',
        'SHELL': '/bin/bash',
        'TERM': 'xterm-256color',
        'LANG': os.environ.get('LANG', 'C.UTF-8'),
        'PATH': os.environ.get('PATH', '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'),
        **request.get('env', {}),
    }
    argv = ['/bin/bash', '-l']
    if os.geteuid() == 0:
        argv = ['/usr/sbin/runuser', '-u', 'user', '--'] + argv
    pid, fd = pty.fork()
    if pid == 0:
        try:
            os.chdir(WORKSPACE)
            os.execve(argv[0], argv, env)
        finally:
            os._exit(127)
    set_winsize(fd, cols, rows)
    response(conn, {'ok': True})

    def pump():
        while True:
            try:
                chunk = os.read(fd, MAX_FRAME)
            except OSError:
                break
            if not chunk:
                break
            try:
                send_frame(conn, FRAME_DATA, chunk)
            except OSError:
                break
        _, status = os.waitpid(pid, 0)
        try:
            send_frame(conn, FRAME_EXIT, struct.pack('>i', os.waitstatus_to_exitcode(status)))
            conn.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass

    output = threading.Thread(target=pump, daemon=True)
    output.start()
    try:
        while output.is_alive():
            kind, size = struct.unpack('>BI', read_exact(reader, 5))
            if size > MAX_FRAME:
                break
            payload = read_exact(reader, size)
            if kind == FRAME_DATA:
                view = memoryview(payload)
                while view:
                    view = view[os.write(fd, view):]
            elif kind == FRAME_RESIZE and size == 4:
                set_winsize(fd, *struct.unpack('>HH', payload))
    except (EOFError, OSError, ValueError, struct.error):
        pass
    finally:
        hang_up(pid)
        output.join(5)
        # A detached process may still hold the pty open; closing the master unblocks the pump.
        os.close(fd)
        output.join(1)


def run_tcp(conn, reader, request):
    port = int(request.get('port', 0))
    # The agent's own port is never tunneled.
    if not 1 <= port <= 65535 or port == server.getsockname()[1]:
        response(conn, {'ok': False, 'error': 'invalid_port'})
        return
    upstream = None
    for host in ('127.0.0.1', '::1'):
        try:
            upstream = socket.create_connection((host, port), timeout=5)
            break
        except OSError:
            pass
    if upstream is None:
        response(conn, {'ok': False, 'error': 'port_closed'})
        return
    upstream.settimeout(None)
    response(conn, {'ok': True})

    def close_both():
        for sock in (upstream, conn):
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    def pump():
        try:
            while True:
                chunk = upstream.recv(65536)
                if not chunk:
                    break
                conn.sendall(chunk)
        except OSError:
            pass
        close_both()

    output = threading.Thread(target=pump, daemon=True)
    output.start()
    try:
        while True:
            chunk = reader.read1(65536)
            if not chunk:
                break
            upstream.sendall(chunk)
    except (OSError, ValueError):
        pass
    finally:
        close_both()
        output.join(5)
        upstream.close()


def handle(conn):
    global TOKEN
    try:
        reader = conn.makefile('rb')
        raw = reader.readline(8 * 1024 * 1024)
        request = json.loads(raw.decode())
        if request.get('token') != TOKEN:
            response(conn, {'ok': False, 'error': 'invalid_token'})
            return
        op = request.get('op')
        if op == 'ping':
            response(conn, {'ok': True})
            return
        if op == 'rekey':
            new_token = request.get('newToken', '')
            if not new_token:
                response(conn, {'ok': False, 'error': 'missing_token'})
                return
            CONFIG['token'] = new_token
            with open('/etc/dock-fast-agent.json', 'w', encoding='utf-8') as config_file:
                json.dump(CONFIG, config_file)
            TOKEN = new_token
            response(conn, {'ok': True})
            return
        if op == 'exec':
            response(conn, run_script(request))
            return
        if op == 'pty':
            run_pty(conn, reader, request)
            return
        if op == 'tcp':
            run_tcp(conn, reader, request)
            return
        if op == 'read':
            path = request.get('path', '')
            if not allowed(path):
                response(conn, {'ok': False, 'error': 'path_denied'})
                return
            with open(path, 'rb') as handle:
                content = handle.read(32 * 1024 * 1024)
            response(conn, {
                'ok': True,
                'content': base64.b64encode(content).decode(),
                'sizeBytes': len(content),
            })
            return
        response(conn, {'ok': False, 'error': 'unsupported_operation'})
    except Exception as exc:
        response(conn, {'ok': False, 'error': str(exc)[:500]})
    finally:
        conn.close()


server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
server.bind(('0.0.0.0', 2222))
server.listen(32)
while True:
    connection, _ = server.accept()
    threading.Thread(target=handle, args=(connection,), daemon=True).start()
`

const fastAgentUnit = `[Unit]
Description=Dock Fast Run guest agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/libexec/dock-fast-agent.py
Restart=on-failure
RestartSec=1

[Install]
WantedBy=multi-user.target
`

// The wire types live in guestagent so the CLI's local engine speaks the same protocol.
type (
	fastAgentRequest  = guestagent.Request
	fastAgentResponse = guestagent.Response
)

// MaxExecOutput bounds stdout and stderr each; the guest keeps the head.
const MaxExecOutput = guestagent.MaxExecOutput

// ErrExecUnavailable marks a machine running without a guest exec agent.
var ErrExecUnavailable = errors.New("exec_unavailable")

func (h *Host) ProvisionFast(ctx context.Context, runtimeID string, res apiv1.Resources, image, token, workspace string, egressAllow []string) (string, error) {
	if err := checkIDs(runtimeID); err != nil {
		return "", err
	}
	if runtimeID == "" || token == "" || workspace == "" {
		return "", errors.New("fast runtime requires runtime id, token, and workspace")
	}
	image, err := ResolveImage(image)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	if _, exists := h.vms[runtimeID]; exists {
		h.mu.Unlock()
		return "", fmt.Errorf("%w: %s", errFastExists, runtimeID)
	}
	h.mu.Unlock()
	if h.pl != nil {
		spec := poolSpec{res: res, image: image}
		key := manifestKey(res, image)
		for range 3 {
			slot, ok := h.pl.pop(key)
			if !ok {
				break
			}
			ip, err := h.claimSlot(ctx, slot, runtimeID, token, egressAllow)
			if err == nil {
				h.refill(key, spec)
				return ip, nil
			}
			if errors.Is(err, errFastExists) {
				return "", err
			}
		}
		h.refill(key, spec)
	}
	return h.provisionWithAgent(ctx, runtimeID, res, image, "", fastSpec{token: token, workspace: workspace, egress: egressAllow, ephemeral: true})
}

// ProvisionWithAgent boots a regular dock with the guest exec agent so
// harness runs can drive it; the token never leaves the host.
func (h *Host) ProvisionWithAgent(ctx context.Context, dockID string, res apiv1.Resources, image, snapshotID string, egressAllow []string) (string, error) {
	if err := checkIDs(dockID, snapshotID); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return h.provisionWithAgent(ctx, dockID, res, image, snapshotID, fastSpec{token: hex.EncodeToString(raw), workspace: "/home/user/work", egress: egressAllow})
}

// SnapshotLoadWithAgent resumes a dock with a freshly keyed guest exec agent.
func (h *Host) SnapshotLoadWithAgent(ctx context.Context, dockID string, res apiv1.Resources, snapshotID string, egressAllow []string) (string, error) {
	if err := checkIDs(dockID, snapshotID); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	h.mu.Lock()
	h.fastSpecs[dockID] = fastSpec{token: hex.EncodeToString(raw), workspace: "/home/user/work", egress: egressAllow}
	h.mu.Unlock()
	ip, err := h.SnapshotLoad(ctx, dockID, res, snapshotID)
	if err != nil {
		h.mu.Lock()
		delete(h.fastSpecs, dockID)
		h.mu.Unlock()
		return "", err
	}
	if err := h.waitFastAgent(ctx, dockID); err != nil {
		_ = h.Destroy(ctx, dockID)
		return "", err
	}
	return ip, nil
}

func (h *Host) provisionWithAgent(ctx context.Context, id string, res apiv1.Resources, image, snapshotID string, spec fastSpec) (string, error) {
	h.mu.Lock()
	h.fastSpecs[id] = spec
	h.mu.Unlock()
	ip, err := h.Provision(ctx, id, res, image, snapshotID)
	if err != nil {
		h.mu.Lock()
		delete(h.fastSpecs, id)
		h.mu.Unlock()
		return "", err
	}
	if err := h.waitFastAgent(ctx, id); err != nil {
		_ = h.Destroy(ctx, id)
		return "", err
	}
	return ip, nil
}

// FastExec runs a bounded script in the guest; env is passed per call and never written to disk.
func (h *Host) FastExec(ctx context.Context, runtimeID, script string, env map[string]string, timeout time.Duration) (apiv1.FastRunExecResponse, error) {
	v, err := h.fastVM(runtimeID)
	if err != nil {
		return apiv1.FastRunExecResponse{}, err
	}
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if v.egressGateway != "" {
		merged := make(map[string]string, len(env)+6)
		for k, val := range env {
			merged[k] = val
		}
		for k, val := range egress.ProxyEnv(v.egressGateway, h.cfg.EgressPort) {
			merged[k] = val
		}
		env = merged
	}
	response, err := h.agentCall()(ctx, v.guestIP, v.fastToken, fastAgentRequest{
		Op:             "exec",
		Script:         script,
		TimeoutSeconds: seconds,
		Env:            env,
		OutputLimit:    MaxExecOutput,
	})
	if err != nil {
		return apiv1.FastRunExecResponse{}, err
	}
	// Older baked agents ignore outputLimit, so the host clips as well.
	stdout, cutOut := clip(response.Stdout, MaxExecOutput)
	stderr, cutErr := clip(response.Stderr, MaxExecOutput)
	return apiv1.FastRunExecResponse{
		RuntimeID: runtimeID,
		ExitCode:  response.ExitCode,
		Stdout:    stdout,
		Stderr:    stderr,
		Truncated: response.Truncated || cutOut || cutErr,
		TimedOut:  response.TimedOut,
	}, nil
}

func clip(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	return strings.ToValidUTF8(s[:limit], ""), true
}

func (h *Host) FastRead(ctx context.Context, runtimeID, path string) (apiv1.FastRunReadResponse, error) {
	v, err := h.fastVM(runtimeID)
	if err != nil {
		return apiv1.FastRunReadResponse{}, err
	}
	response, err := h.agentCall()(ctx, v.guestIP, v.fastToken, fastAgentRequest{Op: "read", Path: path})
	if err != nil {
		return apiv1.FastRunReadResponse{}, err
	}
	content, err := base64.StdEncoding.DecodeString(response.Content)
	if err != nil {
		return apiv1.FastRunReadResponse{}, fmt.Errorf("decode fast artifact: %w", err)
	}
	return apiv1.FastRunReadResponse{RuntimeID: runtimeID, Path: path, Content: content, SizeBytes: int64(len(content))}, nil
}

func (h *Host) fastVM(runtimeID string) (*vm, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vms[runtimeID]
	if !ok || v.fastToken == "" {
		return nil, fmt.Errorf("%w: fast runtime %s not found", ErrExecUnavailable, runtimeID)
	}
	copy := *v
	return &copy, nil
}

func (h *Host) waitFastAgent(ctx context.Context, runtimeID string) error {
	// Cold guests on loaded hosts need a minute past boot for systemd,
	// network-online, and the agent unit; fail long after boot, not mid-boot.
	deadline := time.NewTimer(120 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	ping := h.agentCall()
	for {
		v, err := h.fastVM(runtimeID)
		if err != nil {
			return err
		}
		if _, err := ping(ctx, v.guestIP, v.fastToken, fastAgentRequest{Op: "ping"}); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("fast guest agent readiness timeout")
		case <-tick.C:
		}
	}
}

// rekeyFastAgent rotates a claimed pool VM onto the caller's agent token.
func (h *Host) rekeyFastAgent(ctx context.Context, ip, oldToken, newToken string) error {
	_, err := h.agentCall()(ctx, ip, oldToken, fastAgentRequest{Op: "rekey", NewToken: newToken})
	return err
}

// agentCall dispatches guest-agent calls, overridable in tests.
func (h *Host) agentCall() func(ctx context.Context, ip, token string, req fastAgentRequest) (fastAgentResponse, error) {
	if h.cfg.AgentCall != nil {
		return h.cfg.AgentCall
	}
	return h.fastAgentCall
}

func (h *Host) fastAgentCall(ctx context.Context, ip, token string, request fastAgentRequest) (fastAgentResponse, error) {
	return guestagent.Call(ctx, guestagent.Addr(ip), token, request)
}

func (h *Host) installFastAgent(rootfs, token, workspace string) error {
	target, err := os.MkdirTemp("", "dock-fast-agent")
	if err != nil {
		return err
	}
	defer os.RemoveAll(target)
	if err := h.cfg.Mount.Mount(rootfs, target, false); err != nil {
		return err
	}
	mounted := true
	defer func() {
		if mounted {
			_ = h.cfg.Mount.Unmount(target)
		}
	}()
	g, err := disk.OpenGuest(target)
	if err != nil {
		return err
	}
	defer g.Close()
	workspacePath := strings.TrimPrefix(workspace, "/")
	if err := g.MkdirAll(workspacePath, 0o755); err != nil {
		return err
	}
	if err := g.Chown(workspacePath, h.cfg.UID, h.cfg.GID); err != nil {
		return err
	}
	artifactPath := "run/dock-fast"
	if err := g.MkdirAll(artifactPath, 0o755); err != nil {
		return err
	}
	if err := g.Chown(artifactPath, h.cfg.UID, h.cfg.GID); err != nil {
		return err
	}
	config, err := json.Marshal(map[string]any{
		"token":     token,
		"workspace": workspace,
		"roots":     []string{workspace, "/run/dock-fast"},
	})
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"usr/local/libexec/dock-fast-agent.py":       []byte(fastAgentProgram),
		"etc/dock-fast-agent.json":                   config,
		"etc/systemd/system/dock-fast-agent.service": []byte(fastAgentUnit),
	}
	for relative, content := range files {
		if err := g.WriteFile(relative, content, 0o700); err != nil {
			return err
		}
	}
	wants := "etc/systemd/system/multi-user.target.wants/dock-fast-agent.service"
	if err := g.Symlink("/etc/systemd/system/dock-fast-agent.service", wants); err != nil {
		return err
	}
	_ = g.Close()
	if err := h.cfg.Mount.Unmount(target); err != nil {
		return err
	}
	mounted = false
	return nil
}
