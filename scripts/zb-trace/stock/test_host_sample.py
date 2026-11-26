import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from host_sample import Binding, VMM, read_bindings, sample, snapshot_cache


SERVER = """
import socket
import sys
with socket.socket(socket.AF_UNIX) as listener:
    listener.bind(sys.argv[2])
    listener.listen()
    print('ready', flush=True)
    while True:
        listener.accept()[0].close()
"""


class HostSampleTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def start(self, name, label="--api-sock"):
        path = self.root / name
        process = subprocess.Popen([sys.executable, "-c", SERVER, label, str(path)],
                                   stdout=subprocess.PIPE, text=True)

        def stop():
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
            process.stdout.close()

        self.addCleanup(stop)
        self.assertEqual(process.stdout.readline().strip(), "ready")
        return process, path

    def test_survives_stop_exit_and_replacement(self):
        first, first_socket = self.start("first.socket")
        second, second_socket = self.start("second.socket")
        vmm = VMM()
        self.addCleanup(vmm.close)

        observed = vmm.sample(Binding(1, first_socket, False))
        self.assertEqual((observed["status"], observed["pid"]), ("live", first.pid))
        self.assertGreater(observed["rss_bytes"], 0)
        self.assertGreater(observed["pss_bytes"], 0)
        self.assertIsNone(observed["cpu_cores"])
        self.assertIsNotNone(vmm.sample(Binding(1, first_socket, False))["cpu_cores"])

        first.send_signal(signal.SIGSTOP)
        first_socket.unlink()
        suspended = Binding(1, first_socket, True)
        self.assertEqual(vmm.sample(suspended)["status"], "live")
        with patch("host_sample.rollup", side_effect=PermissionError("fixture")):
            missing = vmm.sample(suspended)
        self.assertEqual(missing["status"], "unknown")
        self.assertIsNone(missing["pss_bytes"])

        first.kill()
        os.waitid(os.P_PID, first.pid, os.WEXITED | os.WNOWAIT)
        missing = vmm.sample(None)
        self.assertEqual(missing["status"], "unknown")
        self.assertIsNone(missing["rss_bytes"])
        with_no_permission = vmm.sample(Binding(1, first_socket, False))
        self.assertEqual(with_no_permission["status"], "unknown")
        exited = vmm.sample(suspended)
        self.assertEqual((exited["pid"], exited["status"], exited["pss_bytes"], exited["rss_bytes"]),
                         (first.pid, "exited", 0, 0))
        self.assertEqual(vmm.sample(suspended)["cpu_cores"], 0)
        first.wait(timeout=5)
        self.assertEqual(vmm.sample(suspended)["status"], "exited")

        pending = vmm.sample(Binding(2, None, False))
        self.assertEqual(pending["status"], "unknown")
        self.assertIsNone(pending["pss_bytes"])
        self.assertEqual(vmm.sample(suspended)["status"], "unknown")
        restored = vmm.sample(Binding(2, second_socket, False))
        self.assertEqual((restored["status"], restored["pid"], restored["generation"]),
                         ("live", second.pid, 2))
        self.assertIsNone(restored["cpu_cores"])
        self.assertEqual(vmm.sample(suspended)["status"], "unknown")

    def test_rejects_wrong_process_label_and_duplicate_socket(self):
        _, path = self.start("unbound.socket", "--unrelated")
        vmm = VMM()
        self.addCleanup(vmm.close)
        self.assertEqual(vmm.sample(Binding(1, path, True))["status"], "unknown")
        binding = dict(generation=1, socket=str(path), allow_zero=False)
        document = self.root / "bindings.json"
        document.write_text(json.dumps(dict(workload=binding, recording=binding)))
        bindings, error = read_bindings(document)
        self.assertEqual(bindings, {})
        self.assertIn("must differ", error)

    def test_records_roles_and_snapshot_cache_independently(self):
        workload, workload_socket = self.start("workload.socket")
        recording, recording_socket = self.start("recording.socket")
        document = self.root / "bindings.json"
        document.write_text(json.dumps({
            role: dict(generation=1, socket=str(path), allow_zero=False)
            for role, path in (("workload", workload_socket), ("recording", recording_socket))
        }))
        snapshot = self.root / "snapshots"
        snapshot.mkdir()
        path = snapshot / "vm_state.bin"
        path.write_bytes(b"x" * 4096)
        shared = snapshot / "inherited.commit"
        shared.write_bytes(b"x" * 4096)
        os.link(shared, self.root / "external.commit")
        output = io.StringIO()
        sample("sandbox-id", document, [snapshot], 1, output)
        row = json.loads(output.getvalue())
        self.assertEqual((row["pid"], row["recording_vmm"]["pid"]),
                         (workload.pid, recording.pid))
        self.assertLessEqual(row["scheduled_ns"], row["begin_ns"])
        self.assertLessEqual(row["begin_ns"], row["end_ns"])
        self.assertEqual(row["snapshot_cache"]["status"], "partial")
        self.assertEqual(len(row["snapshot_cache"]["files"]), 1)
        self.assertEqual(row["snapshot_cache"]["files"][0]["path"], str(path))
        self.assertGreater(row["snapshot_cache"]["observed_cached_bytes"], 0)
        self.assertIsNone(snapshot_cache([])["observed_cached_bytes"])
        layer = self.root / "new.commit"
        layer.write_bytes(b"x" * 4096)
        manifest = self.root / "checkpoint-cache-paths.json"
        manifest.write_text(json.dumps([str(layer), str(layer)]))
        result = snapshot_cache([], manifest)
        self.assertEqual(result["status"], "observed")
        self.assertEqual([f["path"] for f in result["files"]], [str(layer)])
        manifest.write_text("broken")
        self.assertEqual(snapshot_cache([snapshot], manifest)["status"], "partial")


if __name__ == "__main__":
    unittest.main()
