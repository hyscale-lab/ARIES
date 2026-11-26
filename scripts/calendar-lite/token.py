"""The guest retains a secret only in the process address space."""
import secrets
import socket
import os
import sys
path = '/run/calendar-probe/token.sock'
if sys.argv[1:] == ['serve']:
    token = f'{os.getpid()} {secrets.token_hex(32)}'.encode()
    with socket.socket(socket.AF_UNIX) as server:
        server.bind(path)
        server.listen()
        for _ in range(2):
            client, _ = server.accept()
            with client:
                client.sendall(token)
else:
    with socket.socket(socket.AF_UNIX) as client:
        client.connect(path)
        print(client.makefile().read())
