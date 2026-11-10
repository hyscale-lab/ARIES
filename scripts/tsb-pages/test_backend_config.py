"""Reject unsupported observer configurations before filesystem/runtime effects."""
import argparse
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import serve
import suite


class NativeBackendAdmission(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir=suite.REPO / ".cache")
        self.base = Path(self.temporary.name)
        self.config = self.base / "config.json"
        self.output = self.base / "output"

    def tearDown(self):
        self.temporary.cleanup()

    def test_observer_missing_config_is_rejected_before_effects(self):
        with patch("sys.argv", ["serve.py", "--output", str(self.output)]), \
                patch.object(serve.os, "umask") as umask, \
                patch.object(serve, "ThreadingHTTPServer") as server, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                serve.main()
        umask.assert_not_called()
        server.assert_not_called()
        self.assertFalse(self.output.exists())

    def test_observer_rejects_missing_and_obsolete_backends_before_effects(self):
        for config in ({}, {"backend": "external"}, {"backend": "legacy_external"}, [], None):
            with self.subTest(config=config):
                self.config.write_text(json.dumps(config))
                with patch("sys.argv", ["serve.py", "--output", str(self.output),
                                        "--access-dirty-config", str(self.config)]), \
                        patch.object(serve.os, "umask") as umask, \
                        patch.object(serve, "ThreadingHTTPServer") as server:
                    with self.assertRaisesRegex(ValueError, "backend=vmm_owned"):
                        serve.main()
                umask.assert_not_called()
                server.assert_not_called()
                self.assertFalse(self.output.exists())

    def test_prepare_rejects_missing_and_obsolete_backends_before_effects(self):
        for config in (None, {}, {"backend": "external"}, []):
            with self.subTest(config=config):
                path = None
                if config is not None:
                    self.config.write_text(json.dumps(config))
                    path = self.config
                args = argparse.Namespace(root=self.output, access_dirty_config=path)
                with patch.object(suite, "task_catalog") as catalog:
                    with self.assertRaises(ValueError):
                        suite.prepare(args)
                catalog.assert_not_called()
                self.assertFalse(self.output.exists())

    def test_suite_cli_prepare_checks_backend_before_owner_or_output(self):
        self.config.write_text('{"backend":"external"}')
        argv = ["suite.py", "--prepare", "--root", str(self.output),
                "--access-dirty-config", str(self.config)]
        with patch("sys.argv", argv), patch.object(suite.os, "umask") as umask, \
                patch.object(suite.os, "chdir") as chdir, \
                patch.object(suite.pwd, "getpwnam") as owner:
            with self.assertRaisesRegex(ValueError, "backend=vmm_owned"):
                suite.main()
        umask.assert_not_called()
        chdir.assert_not_called()
        owner.assert_not_called()
        self.assertFalse(self.output.exists())

    def test_run_rejects_legacy_manifest_before_lock_or_execution(self):
        (self.output / "manifests").mkdir(parents=True)
        manifest = self.output / "manifests/shard-0.json"
        manifest.write_text(json.dumps({"execution": {"observation_mode": "group"}}))
        with patch("sys.argv", ["suite.py", "--run", "--root", str(self.output)]), \
                patch.object(suite.os, "umask") as umask, \
                patch.object(suite, "run_suite") as run:
            with self.assertRaisesRegex(ValueError, "VMM-owned observer config required"):
                suite.main()
        umask.assert_not_called()
        run.assert_not_called()
        self.assertFalse((self.output / ".suite.lock").exists())

    def test_frozen_config_rejects_changed_backend_or_bytes(self):
        self.config.write_text('{"backend":"vmm_owned"}')
        manifest = {"execution": {"observation_mode": "group", "access_dirty_config": {
            "path": str(self.config), "sha256": suite.digest(self.config)}}}
        self.assertEqual(suite.prepared_vmm_config(manifest)["backend"], "vmm_owned")
        self.config.write_text('{"backend":"vmm_owned", "changed":true}')
        with self.assertRaisesRegex(ValueError, "changed after preparation"):
            suite.prepared_vmm_config(manifest)
        self.config.write_text('{"backend":"external"}')
        with self.assertRaisesRegex(ValueError, "backend=vmm_owned"):
            suite.prepared_vmm_config(manifest)


if __name__ == "__main__":
    unittest.main()
