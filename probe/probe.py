import socket, sys
for spec in sys.argv[1:]:
    label, host, port = spec.split(":")
    s = socket.socket()
    s.settimeout(3)
    try:
        s.connect((host, int(port)))
        print("  %-10s %s:%s OPEN" % (label, host, port))
    except Exception as e:
        print("  %-10s %s:%s BLOCKED (%s)" % (label, host, port, type(e).__name__ + ":" + str(e)[:40]))
    finally:
        s.close()
