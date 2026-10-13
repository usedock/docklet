#!/usr/bin/env bash
set +e
cd "$(dirname "$0")"
P=$PWD
docker version --format '{{.Server.Version}} api {{.Server.APIVersion}}'
iptables --version; sudo iptables -S INPUT | head; sudo iptables -L DOCKER-USER -n | head -5
HOSTIP=$(ip -4 route get 1.1.1.1 | grep -oP 'src \K\S+')
echo "host ip $HOSTIP"
python3 - <<'PY' &
import socket,threading,time
def srv(h,p):
    s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind((h,p)); s.listen(50)
    while True:
        c,_=s.accept(); c.close()
for h,p in (("0.0.0.0",9999),("127.0.0.1",9998)):
    threading.Thread(target=srv,args=(h,p),daemon=True).start()
time.sleep(600)
PY
sleep 1
IMG=python:3.12-alpine
docker pull -q $IMG >/dev/null
check() { # network name, extra run args
  net=$1; shift
  gw=$(docker network inspect $net --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}')
  echo "--- $net gateway=[$gw]"
  docker run --rm --runtime runsc --network $net "$@" -v $P/probe.py:/probe.py:ro $IMG python /probe.py gw:${gw:-none}:9999 d0:172.17.0.1:9999 host:$HOSTIP:9999 gwloop:127.0.0.1:9998 ip:1.1.1.1:443 name:one.one.one.one:443 2>&1 | tail -12
}
echo "===== A: bridge options"
docker network create bprobe-base >/dev/null && check bprobe-base; docker network rm bprobe-base >/dev/null
for opt in "com.docker.network.bridge.gateway_mode_ipv4=isolated" "com.docker.network.bridge.gateway_mode_ipv4=routed" "com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected" "com.docker.network.bridge.inhibit_ipv4=true"; do
  echo "##### $opt"
  docker network create -o $opt bprobe-a 2>&1 | tail -2 && { docker network inspect bprobe-a --format '{{json .IPAM.Config}} {{json .Options}}'; check bprobe-a; ip -br a | grep -E 'br-' ; }
  docker network rm bprobe-a >/dev/null 2>&1
done
echo "##### --internal"
docker network create --internal bprobe-a >/dev/null && check bprobe-a; docker network rm bprobe-a >/dev/null 2>&1

echo "===== B: host INPUT rule via privileged helper"
docker network create -o com.docker.network.bridge.name=dkltest0 bprobe-b >/dev/null
docker build -q -t probe-ipt - <<'DF'
FROM ubuntu:noble
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends iptables >/dev/null
DF
ipt() { docker run --rm --privileged --network host --runtime runc --entrypoint sh probe-ipt -c "$*"; }
ipt 'iptables --version; iptables-legacy --version; ip link show dkltest0 >/dev/null 2>&1; iptables -w -N DOCK-T && iptables -w -A DOCK-T -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT && iptables -w -A DOCK-T -p udp --dport 53 -d 1.1.1.1 -j ACCEPT && iptables -w -A DOCK-T -j DROP && iptables -w -I INPUT 1 -i dkltest0 -j DOCK-T; echo rc=$?'
sudo iptables -S INPUT | head -3
sudo nft list ruleset 2>/dev/null | head -0
check bprobe-b
docker run -d --name pub --runtime runsc --network bprobe-b -p 127.0.0.1:18080:8000 $IMG python -m http.server 8000 >/dev/null
sleep 3
echo "published port from host:"; curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 http://127.0.0.1:18080/
docker exec pub python -c "import socket; s=socket.create_connection(('1.1.1.1',443),3); print('egress ok')"
docker rm -f pub >/dev/null
ipt 'iptables -w -D INPUT -i dkltest0 -j DOCK-T; iptables -w -F DOCK-T; iptables -w -X DOCK-T; echo cleaned rc=$?'
sudo iptables -S INPUT | head -3; sudo iptables -S | grep -c DOCK-T
docker network rm bprobe-b >/dev/null

echo "===== C: in-sandbox iptables from docker exec --privileged"
docker network create bprobe-c >/dev/null
gw=$(docker network inspect bprobe-c --format '{{range .IPAM.Config}}{{.Gateway}}{{end}}')
docker run -d --name cprobe --runtime runsc --network bprobe-c --entrypoint sleep $IMG 600 >/dev/null
docker exec cprobe sh -c 'apk add -q iptables iptables-legacy 2>&1 | tail -2; ls /sbin/iptables*'
docker exec --privileged cprobe sh -c "iptables-legacy -A OUTPUT -d $gw -j DROP; echo add rc=\$?; iptables-legacy -S OUTPUT"
docker cp $P/probe.py cprobe:/probe.py
docker exec cprobe python /probe.py gw:$gw:9999 host:$HOSTIP:9999 ip:1.1.1.1:443
echo "unprivileged root flush:"; docker exec cprobe sh -c 'iptables-legacy -F OUTPUT; echo rc=$?; iptables-legacy -S OUTPUT'
docker exec cprobe python /probe.py gw:$gw:9999
docker restart cprobe >/dev/null; sleep 2
echo "after restart (rules should be gone):"; docker exec cprobe python /probe.py gw:$gw:9999
docker rm -f cprobe >/dev/null; docker network rm bprobe-c >/dev/null
echo "probe done"
