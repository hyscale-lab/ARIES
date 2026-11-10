import json
import unittest
from pathlib import Path
from file_names import FileNames

LAYOUTS = Path(__file__).resolve().parent / "layouts" / "linux-5.10.239"
NAMES = json.loads((LAYOUTS / "names-layout.json").read_text())
BASE = json.loads((LAYOUTS / "base-layout.json").read_text())


class Memory:
    def __init__(self):
        self.meta = BASE
        self.l = BASE["layout"]
        self.data = {}
        self.next = 0x100000
        self.reads = []

    def alloc(self):
        p = self.next
        self.next += 4096
        self.data[p] = bytearray(4096)
        return p

    def put(self, p, n, value):
        base = p & ~4095
        self.data[base][p-base:p-base+n] = value.to_bytes(n, "little")

    def off(self, p, key, value, n=8):
        self.put(p+NAMES["offsets"][key], n, value)

    def raw(self, p, data):
        base = p & ~4095
        self.data[base][p-base:p-base+len(data)] = data

    def v(self, p, n):
        self.reads.append((p, n))
        base = p & ~4095
        if base not in self.data or p-base+n > 4096:
            raise ValueError("metadata page not retained")
        return bytes(self.data[base][p-base:p-base+n])

    def field(self, p, typ, field, size=8):
        return int.from_bytes(self.v(p+self.l[typ][field], size), "little")

    def basic(self, p, typ, field, value, n=8):
        self.put(p+self.l[typ][field], n, value)

    def sb(self, name="ext4"):
        sb, typ, string = self.alloc(), self.alloc(), self.alloc()
        self.raw(string, name.encode()+b"\0")
        self.off(typ, "file_system_type.name", string)
        self.off(sb, "super_block.s_type", typ)
        self.off(sb, "super_block.s_dev", 8 << 20 | 1, 4)
        self.raw(sb+NAMES["offsets"]["super_block.s_id"], b"vda1\0")
        return sb

    def dent(self, name, sb, parent=None, inode=0):
        p, string = self.alloc(), self.alloc()
        self.raw(string, name.encode()+b"\0")
        self.basic(p, "dentry", "d_parent", p if parent is None else parent)
        self.basic(p, "dentry", "d_inode", inode)
        self.off(p, "dentry.d_sb", sb)
        self.off(p, "dentry.d_hash.pprev", 0x1234)
        q = p+self.l["dentry"]["d_name"]
        self.basic(q, "qstr", "len", len(name), 4)
        self.basic(q, "qstr", "name", string)
        return p

    def inode(self, sb, nlink=1):
        p = self.alloc()
        self.off(p, "inode.i_sb", sb)
        self.off(p, "inode.i_ino", 12345)
        self.off(p, "inode.i_nlink", nlink, 4)
        return p

    def mount(self, sb, root, ns, num, parent=None, mountpoint=None):
        p = self.alloc()
        self.off(p, "mount.mnt_parent", parent or p)
        self.off(p, "mount.mnt_mountpoint", mountpoint or root)
        self.off(p, "mount.mnt_ns", ns)
        self.off(p, "mount.mnt_id", num, 4)
        v = p+NAMES["offsets"]["mount.mnt"]
        self.off(v, "vfsmount.mnt_root", root)
        self.off(v, "vfsmount.mnt_sb", sb)
        return p

    def ns(self, root, mounts, ns):
        self.off(ns, "mnt_namespace.root", root)
        self.off(ns, "mnt_namespace.ns.inum", 4026531840, 4)
        self.off(ns, "mnt_namespace.mounts", len(mounts), 4)
        head = ns+NAMES["offsets"]["mnt_namespace.list"]
        links = [m+NAMES["offsets"]["mount.mnt_list"] for m in mounts]
        self.put(head, 8, links[0])
        for i, p in enumerate(links):
            self.put(p, 8, links[i+1] if i+1 < len(links) else head)
        task, proxy = self.alloc(), self.alloc()
        self.off(task, "task_struct.nsproxy", proxy)
        self.off(proxy, "nsproxy.mnt_ns", ns)
        return [{"task_address": task, "pid": 1, "tgid": 1, "comm": "fixture"}]

    def aliases(self, inode, dents):
        nodes = [d+NAMES["offsets"]["dentry.d_u.d_alias"] for d in dents]
        self.off(inode, "inode.i_dentry", nodes[0] if nodes else 0)
        for i, p in enumerate(nodes):
            self.put(p, 8, nodes[i+1] if i+1 < len(nodes) else 0)


class FileNameTests(unittest.TestCase):
    def fixture(self, nlink=1):
        m = Memory()
        ns = m.alloc()
        a, b = m.sb(), m.sb()
        ar, br = m.dent("/", a), m.dent("/", b)
        point, bind = m.dent("mnt", a, ar), m.dent("bind", a, ar)
        sub = m.dent("dir", b, br)
        inode = m.inode(b, nlink)
        file = m.dent("file", b, sub, inode)
        root = m.mount(a, ar, ns, 1)
        second = m.mount(b, br, ns, 2, root, point)
        third = m.mount(b, sub, ns, 3, root, bind)
        tasks = m.ns(root, [root, second, third], ns)
        m.aliases(inode, [file])
        names = FileNames(m, {"names_layout": NAMES})
        names.begin(tasks)
        return m, names, inode, file, br, b

    def test_namespace_mount_crossing_and_bind_subtree(self):
        m, names, inode, file, br, sb = self.fixture()
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "namespace_path")
        self.assertEqual({p["path"] for p in r["namespace_paths"]}, {"/mnt/dir/file", "/bind/file"})
        self.assertEqual(r["aliases"][0]["filesystem_path"], "/dir/file")
        self.assertEqual(r["filesystem"], "ext4")
        self.assertEqual(r["device"]["major"], 8)
        self.assertFalse(names.metadata()["errors"])

    def test_hardlinks_do_not_invent_bind_path(self):
        m, names, inode, file, br, sb = self.fixture()
        other = m.dent("other", sb, br, inode)
        m.aliases(inode, [file, other])
        r = names.resolve(inode)
        self.assertEqual(len(r["aliases"]), 2)
        self.assertEqual({p["path"] for p in r["namespace_paths"]},
                         {"/mnt/dir/file", "/bind/file", "/mnt/other"})

    def test_deleted_alias_kept_but_explicit(self):
        m, names, inode, file, br, sb = self.fixture(0)
        m.off(file, "dentry.d_hash.pprev", 0)
        r = names.resolve(inode)
        self.assertTrue(r["deleted"])
        self.assertTrue(all(p["deleted"] and p["unhashed"] for p in r["namespace_paths"]))

    def test_empty_alias_is_not_missing_metadata(self):
        m, names, inode, file, br, sb = self.fixture()
        m.aliases(inode, [])
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "no_dentry_alias")
        self.assertFalse(r["errors"])
        self.assertEqual(r["inode_number"], 12345)

    def test_missing_parent_retains_suffix_not_complete_path(self):
        m, names, inode, file, br, sb = self.fixture()
        m.basic(file, "dentry", "d_parent", 0xdead000)
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "partial_suffix")
        self.assertEqual(r["aliases"][0]["name_suffix"], "file")
        self.assertIsNone(r["aliases"][0]["filesystem_path"])
        self.assertFalse(r["namespace_paths"])
        self.assertIn("not retained", r["aliases"][0]["error"])

    def test_bad_backlink_never_assigned_a_name(self):
        m, names, inode, file, br, sb = self.fixture()
        m.basic(file, "dentry", "d_inode", inode+8)
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "metadata_error")
        self.assertFalse(r["aliases"])
        self.assertIn("backlink", r["errors"][0]["error"])

    def test_block_device_cache_has_device_identity_not_fake_path(self):
        m, names, inode, file, br, sb = self.fixture()
        fs_type = int.from_bytes(m.v(sb+NAMES["offsets"]["super_block.s_type"],8),"little")
        name_ptr = int.from_bytes(m.v(fs_type+NAMES["offsets"]["file_system_type.name"],8),"little")
        m.raw(name_ptr, b"bdev\0")
        m.off(inode, "inode.i_mode", 0x6000, 2)
        m.off(inode, "inode.i_rdev", 254 << 20 | 16, 4)
        m.aliases(inode, [])
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "internal_backing")
        self.assertEqual(r["backing_kind"], "block_device_cache")
        self.assertEqual(r["special_device"]["major"], 254)
        self.assertEqual(r["special_device"]["minor"], 16)
        self.assertFalse(r["namespace_paths"])

    def test_ext4_buddy_identity_requires_superblock_pointer(self):
        m, names, inode, file, br, sb = self.fixture()
        m.aliases(inode, [])
        private = m.alloc()
        m.off(sb, "super_block.s_fs_info", private)
        m.off(private, "ext4_sb_info.s_buddy_cache", inode)
        r = names.resolve(inode)
        self.assertEqual(r["backing_kind"], "ext4_allocator_buddy_cache")
        self.assertFalse(r["aliases"])
        names.begin([])
        m.off(private, "ext4_sb_info.s_buddy_cache", inode+8)
        r = names.resolve(inode)
        self.assertEqual(r["naming_status"], "no_dentry_alias")
        self.assertNotIn("backing_kind", r)

    def test_boundary_reset_invalidates_name_cache(self):
        m, names, inode, file, br, sb = self.fixture()
        r = names.resolve(inode)
        m.off(inode, "inode.i_nlink", 0, 4)
        self.assertFalse(names.resolve(inode)["deleted"])
        names.begin([])
        self.assertTrue(names.resolve(inode)["deleted"])


if __name__ == "__main__":
    unittest.main()
