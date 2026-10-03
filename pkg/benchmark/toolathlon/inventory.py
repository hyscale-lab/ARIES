"""Inventory of the evaluator runtime in an ARIES Toolathlon sandbox.

Run by the adapter with the image's system Python (never the virtualenv it
inventories): python3 - <workspace> <manifest>. Writes one line per regular
file ("<sha256>  <path>") under the virtualenv, the interpreter's home, the
uv binary, the files uv reads for its configuration, and the top level of the
project directory; one line per symbolic link ("link <path> -> <target>")
under the first two; and prints the manifest's own digest. Bytecode caches
are skipped (the evaluator runs with a private cache prefix and never loads
them), and so are .log files: Toolathlon's excel MCP server writes its log
into the virtual environment while the agent works. Every file read is released from the page cache afterwards
(posix_fadvise DONTNEED), so the inventory does not raise the sandbox's
memory figure -- ARIES's monitor counts the cgroup's cache -- by the size of
the runtime it reads. Standard library only.
"""
import hashlib
import os
import shutil
import sys

WORKSPACE, MANIFEST = sys.argv[1], sys.argv[2]
os.chdir(WORKSPACE)


def fail(message):
    sys.stderr.write(message + "\n")
    sys.exit(3)


uv_bin = shutil.which("uv")
if not uv_bin:
    fail("uv is not on PATH")
venv_python = os.path.join(".venv", "bin", "python")
if not os.access(venv_python, os.X_OK):
    fail("no .venv/bin/python under " + WORKSPACE)
python_home = os.path.dirname(os.path.dirname(os.path.realpath(venv_python)))

roots = [".venv", python_home, uv_bin]
for extra in ["pyproject.toml", "uv.lock", "uv.toml", ".python-version",
              os.path.join(os.path.expanduser("~"), ".config", "uv"), "/etc/uv"]:
    if os.path.lexists(extra):
        roots.append(extra)


def digest(path):
    h = hashlib.sha256()
    fd = os.open(path, os.O_RDONLY)
    try:
        with os.fdopen(fd, "rb", closefd=False) as handle:
            for block in iter(lambda: handle.read(1 << 20), b""):
                h.update(block)
        try:
            os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
        except OSError:
            pass
    finally:
        os.close(fd)
    return h.hexdigest()


files, links = [], []
for root in roots:
    if os.path.islink(root):
        links.append((root, os.readlink(root)))
        continue
    if os.path.isfile(root):
        files.append(root)
        continue
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d != "__pycache__")
        for name in sorted(filenames):
            path = os.path.join(dirpath, name)
            if os.path.islink(path):
                if root in (".venv", python_home):
                    links.append((path, os.readlink(path)))
                continue
            if name.endswith((".pyc", ".log")):
                continue  # bytecode is never loaded (cache prefix); MCP servers log into the venv
            if os.path.isfile(path):
                files.append(path)
top = sorted(entry for entry in os.listdir(".") if os.path.isfile(entry) and not os.path.islink(entry))
files.extend("./" + entry for entry in top)

with open(MANIFEST, "w", encoding="utf-8") as out:
    for path in sorted(set(files)):
        out.write("%s  %s\n" % (digest(path), path))
    for path, target in sorted(set(links)):
        out.write("link %s -> %s\n" % (path, target))
print(digest(MANIFEST))
