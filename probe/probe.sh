#!/usr/bin/env bash
set +e
cd "$(dirname "$0")"
P=$PWD
HOSTIP=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')
python3 - <<'PY' &
import socket,threading,time
def srv(h,p):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind((h,p)); s.listen(50)
    while True:
        c,_=s.accept(); c.close()
for h,p in (("0.0.0.0",9999),):
    threading.Thread(target=srv,args=(h,p),daemon=True).start()
time.sleep(600)
PY
sleep 1
echo "===== C: in-sandbox iptables from docker exec --privileged (ubuntu)"
docker build -q -t probe-ipt - <<'DF'
FROM ubuntu:noble
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends iptables python3 sudo >/dev/null && useradd -u 1001 -m user && echo "user ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/user
DF
docker network create bprobe-c >/dev/null
gw=$(docker network inspect bprobe-c --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}')
docker run -d --name cprobe --runtime runsc --network bprobe-c --user 1001:1001 --entrypoint sleep probe-ipt 600 >/dev/null
docker cp $P/probe.py cprobe:/tmp/probe.py
docker exec --user 0 --privileged cprobe sh -c "iptables-legacy -A OUTPUT -d $gw -j DROP; echo add rc=\$?; iptables-legacy -S OUTPUT; iptables-nft -A OUTPUT -d 9.9.9.9 -j DROP; echo nft rc=\$?"
docker exec cprobe python3 /tmp/probe.py gw:$gw:9999 host:$HOSTIP:9999 ip:1.1.1.1:443
echo "--- as user 1001 sudo: flush"
docker exec cprobe sudo iptables-legacy -F OUTPUT; echo rc=$?
docker exec cprobe sudo iptables-legacy -S OUTPUT
docker exec cprobe python3 /tmp/probe.py gw:$gw:9999
echo "--- exec --user 0 without --privileged flush"
docker exec --user 0 cprobe iptables-legacy -F OUTPUT; echo rc=$?
docker restart cprobe >/dev/null; sleep 2
echo "after restart (rules gone?):"; docker exec cprobe python3 /tmp/probe.py gw:$gw:9999
docker rm -f cprobe >/dev/null; docker network rm bprobe-c >/dev/null
echo "probe done"
