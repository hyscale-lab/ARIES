"""Add the file-operations seam to the pinned Hermes before it starts.

Hermes builds ShellFileOperations for every backend, so a backend cannot supply
typed file operations. This adds BaseEnvironment.get_file_operations(), which
returns None by default, and makes tools/file_tools.py ask the environment
first. It is the same two-file change proposed upstream; delete this script
once the pin carries it.

Every anchor is checked before anything is written, and a missing anchor exits
non-zero, so a moved pin stops the container instead of silently leaving the
file tools on the shell. A second run finds the seam already present and
changes nothing.
"""

import sys

METHOD = '''    def get_file_operations(self):
        """Return a FileOperations bound to this environment, or None to use
        ShellFileOperations. Backends with a native file API override this."""
        return None

'''

EDITS = (
    (
        "tools/environments/base.py",
        "    def _before_execute(self) -> None:\n",
        METHOD + "    def _before_execute(self) -> None:\n",
    ),
    (
        "tools/file_tools.py",
        "    file_ops = ShellFileOperations(terminal_env)\n",
        "    file_ops = terminal_env.get_file_operations() or ShellFileOperations(terminal_env)\n",
    ),
)


def main(root):
    pending = []
    for name, anchor, replacement in EDITS:
        path = f"{root}/{name}"
        with open(path, encoding="utf-8") as source:
            text = source.read()
        if replacement in text:
            continue
        if text.count(anchor) != 1:
            print(f"ARIES: Hermes seam anchor not found exactly once in {path}", file=sys.stderr)
            return 1
        pending.append((path, text.replace(anchor, replacement)))
    for path, text in pending:
        with open(path, "w", encoding="utf-8") as target:
            target.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "/opt/hermes"))
