#!/usr/bin/env python3
"""Synchronous phase marker for the experimental UFFD handler."""
import argparse
import json
import socket


def send(path, command='status', phase=None, tool_id=None):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(15)
        conn.connect(path)
        conn.sendall((json.dumps({'command':command,'phase':phase,'tool_id':tool_id})+'\n').encode())
        data=bytearray()
        while b'\n' not in data:
            part=conn.recv(4096)
            if not part: raise RuntimeError('EOF awaiting control acknowledgement')
            data.extend(part)
        result=json.loads(data.split(b'\n')[0])
        if not result.get('ok'): raise RuntimeError(result.get('error'))
        return result


if __name__ == '__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--socket',required=True)
    p.add_argument('--phase')
    p.add_argument('--tool-id')
    p.add_argument('--stop',action='store_true')
    a=p.parse_args()
    print(json.dumps(send(a.socket,'stop' if a.stop else 'phase' if a.phase else 'status',a.phase,a.tool_id)))
