"""Paused guest file-cache naming from inodes, dentries and mount namespaces.

All guest reads use the owning decoder's retained metadata reader. No guest
commands or file payload reads. Paths describe mount-tree locations in a named
mount namespace, not guaranteed visibility after overmounts or overlay aliases.
"""
import collections
import json
from pathlib import Path


class FileNames:
    def __init__(self, decoder, config=None):
        self.d = decoder
        config = config if config is not None else decoder.config
        source = config.get("names_layout_data", config.get("names_layout"))
        if source is None:
            source = json.loads(Path(config["names_layout_path"]).read_text())
        self.layout = source
        self.o = source["offsets"]
        if source.get("schema") != "aries.file-names-layout.v1":
            raise ValueError("unsupported file-name layout schema")
        if not all(isinstance(v, int) and not isinstance(v, bool) and 0 <= v < (1 << 20) for v in self.o.values()):
            raise ValueError("invalid file-name layout offsets")
        expected = decoder.meta.get("kernel_sha256")
        if expected and source.get("kernel_sha256") != expected:
            raise ValueError("file-name layout kernel hash mismatch")
        self.max_aliases = int(config.get("names_max_aliases", 256))
        self.max_parents = int(config.get("names_max_parents", 256))
        self.max_mounts = int(config.get("names_max_mounts", 8192))
        if min(self.max_aliases, self.max_parents, self.max_mounts) < 1:
            raise ValueError("file-name traversal limits must be positive")
        self.reset()

    def reset(self):
        self.cache = {}
        self.dentries = {}
        self.mounts = {}
        self.namespaces = {}
        self.sb_mounts = collections.defaultdict(list)
        self.errors = []

    def read(self, address, length):
        if not address:
            raise ValueError("null metadata pointer")
        return self.d.v(address, length)

    def field(self, address, key, size=8):
        return int.from_bytes(self.read(address + self.o[key], size), "little")

    def cstring(self, address, limit=256):
        if not address:
            return ""
        data = bytearray()
        while len(data) < limit:
            amount = min(32, limit-len(data), 4096-((address+len(data)) & 4095))
            part = self.read(address+len(data), amount)
            if b"\0" in part:
                data.extend(part.split(b"\0", 1)[0])
                return data.decode("utf-8", errors="replace")
            data.extend(part)
        raise ValueError("unterminated metadata string")

    def dentry(self, address):
        if address in self.dentries:
            return self.dentries[address]
        parent = self.d.field(address, "dentry", "d_parent")
        q = address + self.d.l["dentry"]["d_name"]
        size = self.d.field(q, "qstr", "len", 4)
        if size > 255:
            raise ValueError("invalid dentry name length")
        pointer = self.d.field(q, "qstr", "name")
        name = self.read(pointer, size).decode("utf-8", errors="replace") if size else ""
        row = {
            "address": address, "parent": parent, "name": name,
            "inode": self.d.field(address, "dentry", "d_inode"),
            "superblock": self.field(address, "dentry.d_sb"),
            "flags": self.field(address, "dentry.d_flags", 4),
            "unhashed": self.field(address, "dentry.d_hash.pprev") == 0,
        }
        self.dentries[address] = row
        return row

    def _mount(self, address):
        if address in self.mounts:
            return self.mounts[address]
        vfsmount = address + self.o["mount.mnt"]
        row = {
            "address": address,
            "parent": self.field(address, "mount.mnt_parent"),
            "mountpoint": self.field(address, "mount.mnt_mountpoint"),
            "root": self.field(vfsmount, "vfsmount.mnt_root"),
            "superblock": self.field(vfsmount, "vfsmount.mnt_sb"),
            "namespace_address": self.field(address, "mount.mnt_ns"),
            "mount_id": self.field(address, "mount.mnt_id", 4),
        }
        self.mounts[address] = row
        self.sb_mounts[row["superblock"]].append(row)
        return row

    def _namespace(self, address):
        if address in self.namespaces:
            return self.namespaces[address]
        row = {
            "address": address,
            "namespace_inum": self.field(address, "mnt_namespace.ns.inum", 4),
            "root_mount": self.field(address, "mnt_namespace.root"),
            "declared_mounts": self.field(address, "mnt_namespace.mounts", 4),
            "processes": [], "mount_addresses": [], "errors": [],
        }
        self.namespaces[address] = row
        head = address + self.o["mnt_namespace.list"]
        pointer = int.from_bytes(self.read(head, 8), "little")
        seen = set()
        try:
            while pointer != head:
                if pointer in seen:
                    raise ValueError("mount namespace list cycle")
                if len(seen) >= self.max_mounts:
                    raise ValueError("mount namespace traversal limit")
                seen.add(pointer)
                m = self._mount(pointer-self.o["mount.mnt_list"])
                if m["namespace_address"] != address:
                    raise ValueError("mount namespace backlink mismatch")
                row["mount_addresses"].append(m["address"])
                pointer = int.from_bytes(self.read(pointer, 8), "little")
            if row["root_mount"] not in row["mount_addresses"]:
                raise ValueError("namespace root absent from mount list")
            if len(seen) != row["declared_mounts"]:
                raise ValueError("namespace mount count mismatch")
        except (ValueError, OSError) as error:
            row["errors"].append(str(error))
            self.errors.append({"phase": "mount_namespace", "namespace_address": address,
                                "error": str(error)})
        return row

    def begin(self, tasks, vmas=None):
        self.reset()
        for task in tasks:
            try:
                proxy = self.field(task["task_address"], "task_struct.nsproxy")
                if not proxy:
                    continue
                ns = self.field(proxy, "nsproxy.mnt_ns")
                if not ns:
                    continue
                row = self._namespace(ns)
                row["processes"].append({k: task[k] for k in ("pid", "tgid", "comm")})
            except (ValueError, OSError) as error:
                self.errors.append({"phase": "task_namespace", "pid": task["pid"],
                                    "error": str(error)})
        # VMA mount objects provide an additional cross-check and may expose
        # detached mounts. They never get assigned a guessed namespace.
        for vma in vmas or []:
            file = vma.get("file_address")
            if not file:
                continue
            try:
                path = file+self.d.l["file"]["f_path"]
                vfsmount = self.d.field(path, "path", "mnt")
                if vfsmount:
                    self._mount(vfsmount-self.o["mount.mnt"])
            except (ValueError, OSError) as error:
                self.errors.append({"phase": "vma_mount", "file_address": file,
                                    "error": str(error)})
        return self.metadata()

    def _chain(self, start):
        names = []
        seen = set()
        at = start
        result = {"name_suffix": "", "filesystem_path": None,
                  "chain": [], "error": None}
        try:
            for _ in range(self.max_parents):
                if at in seen:
                    raise ValueError("dentry parent cycle")
                seen.add(at)
                row = self.dentry(at)
                result["chain"].append(at)
                if row["parent"] == at:
                    result["filesystem_path"] = "/" + "/".join(reversed(names))
                    break
                names.append(row["name"])
                at = row["parent"]
            else:
                raise ValueError("dentry parent traversal limit")
        except (ValueError, OSError) as error:
            result["error"] = str(error)
        result["name_suffix"] = "/".join(reversed(names))
        return result

    def _relative(self, start, root):
        names = []
        seen = set()
        at = start
        for _ in range(self.max_parents):
            if at == root:
                return names
            if at in seen:
                raise ValueError("dentry parent cycle")
            seen.add(at)
            row = self.dentry(at)
            if row["parent"] == at:
                return None  # Alias is outside this bind-mount's subtree.
            names.append(row["name"])
            at = row["parent"]
        raise ValueError("dentry parent traversal limit")

    def _namespace_path(self, dentry, mount, namespace):
        names = []
        seen = set()
        at = dentry
        current = mount
        for _ in range(self.max_parents):
            key = (current["address"], at)
            if key in seen:
                raise ValueError("mount parent cycle")
            seen.add(key)
            rel = self._relative(at, current["root"])
            if rel is None:
                return None
            names.extend(rel)
            if current["address"] == namespace["root_mount"]:
                return "/" + "/".join(reversed(names))
            if current["parent"] == current["address"]:
                return None
            at = current["mountpoint"]
            current = self._mount(current["parent"])
            if current["namespace_address"] != namespace["address"]:
                raise ValueError("mount parent namespace mismatch")
        raise ValueError("mount parent traversal limit")

    def resolve(self, inode):
        if inode in self.cache:
            return self.cache[inode]
        result = {"inode_address": inode, "aliases": [], "namespace_paths": [],
                  "errors": [], "naming_status": "unresolved"}
        self.cache[inode] = result
        try:
            sb = self.field(inode, "inode.i_sb")
            dev = self.field(sb, "super_block.s_dev", 4)
            fs_type = self.field(sb, "super_block.s_type")
            fs_name = self.cstring(self.field(fs_type, "file_system_type.name"))
            sbid = self.read(sb+self.o["super_block.s_id"], 32).split(b"\0", 1)[0].decode(errors="replace")
            nlink = self.field(inode, "inode.i_nlink", 4)
            result.update({
                "inode_number": self.field(inode, "inode.i_ino"),
                "inode_generation": self.field(inode, "inode.i_generation", 4),
                "inode_mode": self.field(inode, "inode.i_mode", 2),
                "superblock_address": sb, "filesystem": fs_name,
                "superblock_id": sbid, "device": {"raw": dev, "major": dev >> 20,
                                                 "minor": dev & ((1 << 20)-1)},
                "link_count": nlink, "deleted": nlink == 0,
            })
            if (result["inode_mode"] & 0xf000) in (0x6000, 0x2000):
                rdev = self.field(inode, "inode.i_rdev", 4)
                result["special_device"] = {"raw": rdev, "major": rdev >> 20,
                                            "minor": rdev & ((1 << 20)-1)}
            ptr = self.field(inode, "inode.i_dentry")
            seen = set()
            path_seen = set()
            while ptr:
                if ptr in seen:
                    raise ValueError("inode alias list cycle")
                if len(seen) >= self.max_aliases:
                    raise ValueError("inode alias traversal limit")
                seen.add(ptr)
                dent = ptr-self.o["dentry.d_u.d_alias"]
                drow = self.dentry(dent)
                if drow["inode"] != inode or drow["superblock"] != sb:
                    raise ValueError("inode/dentry alias backlink mismatch")
                chain = self._chain(dent)
                alias = {
                    "dentry_address": dent,
                    "name_suffix": chain["name_suffix"],
                    "filesystem_path": chain["filesystem_path"],
                    "parent_chain_complete": chain["filesystem_path"] is not None,
                    "unhashed": drow["unhashed"],
                    "disconnected": bool(drow["flags"] & self.o["constant.DCACHE_DISCONNECTED"]),
                    "deleted": nlink == 0,
                    "error": chain["error"],
                }
                result["aliases"].append(alias)
                for mount in list(self.sb_mounts.get(sb, [])):
                    ns = self.namespaces.get(mount["namespace_address"])
                    if ns is None:
                        continue
                    try:
                        path = self._namespace_path(dent, mount, ns)
                        if path is None:
                            continue
                        key = (ns["namespace_inum"], mount["mount_id"], path)
                        if key in path_seen:
                            continue
                        path_seen.add(key)
                        result["namespace_paths"].append({
                            "path": path, "namespace_inum": ns["namespace_inum"],
                            "namespace_address": ns["address"], "mount_id": mount["mount_id"],
                            "dentry_address": dent, "deleted": nlink == 0,
                            "unhashed": drow["unhashed"],
                            "disconnected": alias["disconnected"],
                            "scope": "mount-namespace-root; visibility through overmounts/overlay not asserted",
                            "processes": ns["processes"],
                        })
                    except (ValueError, OSError) as error:
                        result["errors"].append({"phase": "namespace_path",
                                                "mount_id": mount["mount_id"], "error": str(error)})
                ptr = int.from_bytes(self.read(ptr, 8), "little")
            if not seen:
                result["naming_status"] = "no_dentry_alias"
                result["unnamed_reason"] = "inode_has_no_cached_dentry_alias"
                if nlink == 0:
                    result["unnamed_reason"] = "deleted_inode_without_cached_alias"
                if fs_name == "bdev" and (result["inode_mode"] & 0xf000) == 0x6000:
                    result["naming_status"] = "internal_backing"
                    result["backing_kind"] = "block_device_cache"
                    result["unnamed_reason"] = "internal_block_device_inode_has_no_ordinary_file_path"
                elif fs_name == "ext4":
                    try:
                        private = self.field(sb, "super_block.s_fs_info")
                        if private and self.field(private, "ext4_sb_info.s_buddy_cache") == inode:
                            result["naming_status"] = "internal_backing"
                            result["backing_kind"] = "ext4_allocator_buddy_cache"
                            result["unnamed_reason"] = "superblock_buddy_cache_pointer_matches_internal_inode"
                    except (ValueError, OSError) as error:
                        result["errors"].append({"phase": "internal_inode_identity",
                                                 "error": str(error)})
            elif result["namespace_paths"]:
                result["naming_status"] = "namespace_path"
            elif any(a["filesystem_path"] is not None for a in result["aliases"]):
                result["naming_status"] = "filesystem_relative_path"
            elif any(a["name_suffix"] for a in result["aliases"]):
                result["naming_status"] = "partial_suffix"
            else:
                result["naming_status"] = "unresolved_alias"
        except (ValueError, OSError) as error:
            result["errors"].append({"phase": "inode_alias", "error": str(error)})
            result["naming_status"] = "partial_error" if result["aliases"] else "metadata_error"
        return result

    def metadata(self):
        return {
            "schema": "aries.file-name-metadata.v1",
            "layout": self.layout,
            "namespaces": list(self.namespaces.values()),
            "mounts": list(self.mounts.values()),
            "errors": list(self.errors),
            "limits": {"aliases": self.max_aliases, "parents": self.max_parents,
                       "mounts_per_namespace": self.max_mounts},
            "scope": "filesystem identities and mount-tree paths; no claim that a path remains reachable through overmounts, overlay copy-up, rename, or after resume",
        }
