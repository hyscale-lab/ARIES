"""Read namespace topology and try literal loopback socket and route lookups."""
import errno
import ipaddress
import json
import os
from pathlib import Path
import socket
import struct
import sys

PORT = 5328
TARGET = '::1'


def kernel_route():
    # RTM_GETROUTE for the exact, unbound IPv6 destination. No table mutations.
    head = struct.Struct('=IHHII')
    route = struct.Struct('=BBBBBBBBI')
    attr = struct.Struct('=HH')
    dst = socket.inet_pton(socket.AF_INET6, TARGET)
    body = route.pack(socket.AF_INET6, 128, 0, 0, 0, 0, 0, 0, 0) + attr.pack(20, 1) + dst
    with socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 0) as nl:
        nl.settimeout(2)
        nl.sendto(head.pack(head.size+len(body), 26, 1, 1, 0)+body, (0,0))
        reply = nl.recv(65536)
    size, kind, flags, seq, pid = head.unpack_from(reply)
    if kind == 2:
        error = -struct.unpack_from('=i', reply, head.size)[0]
        return {'errno': error, 'name': errno.errorcode.get(error)}
    assert kind == 24 and seq == 1
    af, plen, slen, tos, table, proto, scope, rtype, rflags = route.unpack_from(reply, head.size)
    result = {'table':table,'type':rtype,'scope':scope,'dst_prefix':plen,'protocol':proto}
    index = head.size + route.size
    while index < size:
        n,k=attr.unpack_from(reply,index)
        assert n>=attr.size
        value=reply[index+attr.size:index+n]
        if k in (1,2,5,7) and len(value)==16:
            result[{1:'destination',2:'source',5:'gateway',7:'preferred_source'}[k]]=str(ipaddress.IPv6Address(value))
        if k==4: result['egress_interface']=socket.if_indextoname(int.from_bytes(value,sys.byteorder))
        index+=(n+3)&~3
    return result


def probe():
    rows=Path('/proc/net/if_inet6').read_text().splitlines()
    result={'kernel':os.uname().release,'python':sys.version.split()[0],'netns':os.readlink('/proc/self/ns/net'),
            'interfaces':socket.if_nameindex(),'ipv6_interface_addresses':rows,
            'ipv6_lo_addresses':[r for r in rows if r.split()[-1]=='lo'],
            'ipv6_route_table_proc':Path('/proc/net/ipv6_route').read_text().splitlines(),
            'ipv4_route_table_proc':Path('/proc/net/route').read_text().splitlines(),
            'hosts_file':Path('/etc/hosts').read_text(),'resolv_conf':Path('/etc/resolv.conf').read_text(),
            'ipv6_route_to_literal':kernel_route(),'source_port':PORT}
    result['sysctls']={n:int(Path('/proc/sys/net/ipv6',p).read_text()) for n,p in {'lo':'conf/lo/disable_ipv6','all':'conf/all/disable_ipv6','bindv6only':'bindv6only'}.items()}
    result['address_generation_mode']={k:int(Path('/proc/sys/net/ipv6/conf',k,'addr_gen_mode').read_text()) for k in ['default']+[name for _,name in result['interfaces']]}
    result['resolver']={name:[[af.name,addr[0]] for af,_,_,_,addr in socket.getaddrinfo(name,PORT,type=socket.SOCK_STREAM)] for name in ('127.0.0.1','localhost','::1')}
    with socket.socket(socket.AF_INET6) as s: s.bind(('::',0))
    with socket.socket(socket.AF_INET) as s: s.bind(('127.0.0.1',0))
    result.update(ipv6_wildcard_bind=True,ipv4_loopback_bind=True)
    for kind in (socket.SOCK_STREAM,socket.SOCK_DGRAM):
        with socket.socket(socket.AF_INET6,kind) as s:
            s.settimeout(2)
            err=s.connect_ex((TARGET,PORT))
            result[kind.name]={'errno':err,'name':errno.errorcode.get(err),'local_endpoint':s.getsockname()}
    result['early_ipv6_connect_errno']=result['SOCK_STREAM']['errno']
    return result


if __name__=='__main__':
    result = probe()
    print(json.dumps(result,sort_keys=True))
    if sys.argv[1:] in (["--require-omitted"], ["--require-ipv4-addresses"]):
        assert not result["ipv6_lo_addresses"] and not any(result["sysctls"].values())

    if sys.argv[1:] == ["--require-ipv4-addresses"]:
        assert not result["ipv6_interface_addresses"]
        assert result["address_generation_mode"]["eth0"] == result["address_generation_mode"]["default"] == 1
        assert result["early_ipv6_connect_errno"] == errno.EADDRNOTAVAIL
