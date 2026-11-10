#!/usr/bin/env python3
import array
import copy
import json
import os
from pathlib import Path
import socket
import threading
import unittest
import handler


class HandlerTests(unittest.TestCase):
    def setUp(self):
        self.maps=[{'base_host_virt_addr':0x100000,'size':8192,'offset':0,'page_size':4096},
                   {'base_host_virt_addr':0x200000,'size':4096,'offset':8192,'page_size':4096}]

    def test_snapshot_identity_ignores_host_gap(self):
        m=handler.validate_mappings(self.maps,12288)
        r=handler.mapping_for(0x200123,m)
        self.assertEqual(r['snapshot_page'],2)
        self.assertNotIn('guest_page_address',r)

    def test_explicit_guest_gap_preserved(self):
        m=handler.validate_mappings(self.maps,12288,[0,0x100000000])
        self.assertEqual(handler.mapping_for(0x200123,m)['guest_page_address'],0x100000000)

    def test_mapping_failures(self):
        for key,value in [('size',0),('offset',1),('page_size',2097152),('base_host_virt_addr',0x200000)]:
            with self.subTest(key=key):
                m=copy.deepcopy(self.maps);m[0][key]=value
                with self.assertRaises(ValueError):handler.validate_mappings(m,12288)
        with self.assertRaises(ValueError):handler.mapping_for(0,self.maps)
        with self.assertRaises(ValueError):handler.validate_mappings(self.maps,16384)
        with self.assertRaises(ValueError):handler.validate_mappings(self.maps,12288,[0])

    def test_fragmented_handshake(self):
        left,right=socket.socketpair();r,w=os.pipe()
        blob=json.dumps(self.maps).encode()
        def sender():
            left.sendmsg([blob[:15]],[(socket.SOL_SOCKET,socket.SCM_RIGHTS,array.array('i',[r]))])
            left.sendall(blob[15:]);left.close()
        t=threading.Thread(target=sender);t.start()
        try:
            mappings,fd=handler.receive_handshake(right)
            self.assertEqual(mappings,self.maps)
            os.write(w,b'x');self.assertEqual(os.read(fd,1),b'x');os.close(fd)
        finally:
            right.close();os.close(r);os.close(w);t.join()


if __name__=='__main__':unittest.main()
