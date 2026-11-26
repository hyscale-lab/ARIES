#!/usr/bin/env bash
set -euo pipefail
task=$1 phase=$2 sandbox=$3
b=/local/zb-study3-stock/local-sol
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
if [[ "$phase" == upload ]]; then
    (cd "$b/$task" && sha256sum -c environment.sha256)
    "$AENV_CLI" upload "$sandbox" "$b/$task/environment.tar" /tmp/social-env.tar
    "$AENV_CLI" upload "$sandbox" "$b/social-wheels.tar" /tmp/social-wheels.tar
    guest sh -ec 'cd /workload/rootfs; printf "127.0.0.1 localhost\n::1 localhost\n" >etc/hosts; tar --no-same-owner -xf /tmp/social-env.tar -C tmp_workspace; mkdir tmp/social-wheels; tar xf /tmp/social-wheels.tar -C tmp/social-wheels; mkdir -p root/skills; cp -r tmp_workspace/.wildclaw/skills/. root/skills/; cp tmp_workspace/USER.md root/USER.md; rm /tmp/social-env.tar /tmp/social-wheels.tar'
    exit
fi
[[ "$phase" == ready ]]
pid=$(guest runc state zb-workload | jq -er .pid)
guest nsenter -t "$pid" -n chroot /workload/rootfs /usr/bin/python3 -c 'import fcntl,socket,struct; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); req=struct.pack("16sH22x",b"lo",0); flags=struct.unpack("16sH22x",fcntl.ioctl(s,0x8913,req))[1]; fcntl.ioctl(s,0x8914,struct.pack("16sH22x",b"lo",flags|1))'
guest runc exec zb-workload python3 -c 'import socket; assert socket.if_nameindex()==[(1,"lo")]; assert not any(l.split()[1]=="00000000" for l in open("/proc/net/route").readlines()[1:])'
guest runc exec zb-workload sh -ec 'cd /tmp/social-wheels; sha256sum -c SHA256SUMS; test ! -e /tmp/wildclaw-harbor-warmup.done'
guest runc exec --env PIP_NO_INDEX=1 --env PIP_FIND_LINKS=/tmp/social-wheels zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
guest runc exec zb-workload sh -ec 'test -e /tmp/wildclaw-harbor-warmup.done; python3 -c "import urllib.request; assert urllib.request.urlopen(\"http://localhost:9110/openapi.json\").status==200"; if test -f /tmp_workspace/mock_services/contacts/server.py; then curl -fsS http://localhost:9103/openapi.json >/dev/null; fi; test ! -e /tmp_workspace/tmp; rm -r /tmp/social-wheels'
