#!/usr/bin/env python3
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
