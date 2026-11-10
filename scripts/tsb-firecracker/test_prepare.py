"""Real Git reconstruction fixtures; all repositories stay in the workspace cache."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("prepare.py")
CACHE = SCRIPT.parents[2] / ".cache" / "tracing-iteration" / "prepare-tests"


class PreparationTests(unittest.TestCase):
    def setUp(self):
        CACHE.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=CACHE)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        subprocess.run(["git", "init", "-q", str(self.source)], check=True)
        self.helper = self.root / "helper"
        self.helper.mkdir()
        shutil.copyfile(SCRIPT, self.helper / "prepare.py")
        self.input = self.source / "tracer.txt"
        self.input.write_text("upstream\n")
        self.commit("upstream")
        base = self.git("rev-parse", "HEAD")
        base_tree = self.git("write-tree")
        self.input.write_text("published tracer\n")
        patch = self.git("diff", "--binary").encode()
        (self.helper / "baseline.patch").write_bytes(patch)
        self.commit("published tracer")
        published = self.git("rev-parse", "HEAD")
        published_tree = self.git("write-tree")
        self.input.write_text("optimized tracer\n")
        overhead = self.git("diff", "--binary").encode()
        (self.helper / "overhead.patch").write_bytes(overhead)
        self.git("add", "tracer.txt")
        self.optimized_tree = self.git("write-tree")
        self.base_tree = base_tree
        self.versions = {
            "upstream_repository": str(self.source),
            "base_revision": base, "revision": published, "tree": published_tree,
            "patch": "baseline.patch", "patch_sha256": hashlib.sha256(patch).hexdigest(),
            "overhead_patch": {"file": "overhead.patch", "sha256": hashlib.sha256(overhead).hexdigest(),
                               "tree": self.optimized_tree},
        }
        self.save_versions()

    def git(self, *args, directory=None):
        return subprocess.check_output(["git", "-C", str(directory or self.source), *args],
                                       text=True).rstrip("\n") + ("\n" if args[0] == "diff" else "")

    def commit(self, message):
        self.git("add", "tracer.txt")
        self.git("-c", "user.name=Tracing fixture", "-c", "user.email=tracing@example.invalid",
                 "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
                 "commit", "-q", "-m", message)

    def save_versions(self):
        (self.helper / "versions.json").write_text(json.dumps(self.versions))

    def prepare(self, *options):
        destination = self.root / "prepared"
        result = subprocess.run([sys.executable, str(self.helper / "prepare.py"), str(destination),
                                 "--source", str(self.source), *options],
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        return result, destination

    def test_default_verifies_baseline_then_reconstructs_optimized_tree(self):
        result, destination = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((destination / "tracer.txt").read_text(), "optimized tracer\n")
        self.assertEqual(self.git("write-tree", directory=destination), self.optimized_tree)

    def test_baseline_keeps_published_tree_without_reading_corrupt_overlay(self):
        (self.helper / "overhead.patch").write_text("corrupt optimization")
        result, destination = self.prepare("--baseline")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((destination / "tracer.txt").read_text(), "published tracer\n")
        self.assertEqual(self.git("write-tree", directory=destination), self.versions["tree"])

    def test_corrupt_baseline_patch_fails_before_clone(self):
        (self.helper / "baseline.patch").write_text("corrupt baseline")
        result, destination = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("source patch differs from its pinned SHA256", result.stderr)
        self.assertFalse(destination.exists())

    def test_corrupt_optimization_patch_fails_before_clone(self):
        (self.helper / "overhead.patch").write_text("corrupt optimization")
        result, destination = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("overhead patch differs from its pinned SHA256", result.stderr)
        self.assertFalse(destination.exists())

    def test_baseline_tree_mismatch_prevents_applying_optimization(self):
        self.versions["tree"] = self.base_tree
        self.save_versions()
        result, destination = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reconstructed Firecracker tree differs", result.stderr)
        self.assertEqual((destination / "tracer.txt").read_text(), "published tracer\n")

    def test_optimized_tree_mismatch_fails_before_native_build(self):
        self.versions["overhead_patch"]["tree"] = self.versions["tree"]
        self.save_versions()
        result, _ = self.prepare("--build")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("optimized Firecracker tree differs", result.stderr)
        self.assertNotIn("Prepared Firecracker", result.stdout)


if __name__ == "__main__":
    unittest.main()
