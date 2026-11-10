import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from make_config import build_config

class ConfigTests(unittest.TestCase):
 def test_all_layouts_must_match_supplied_elf_hash(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);kernel=root/"kernel";kernel.write_bytes(b"\x7fELFconfiguration-hash-fixture")
   digest=hashlib.sha256(kernel.read_bytes()).hexdigest()
   for name in ("base-layout.json","owner-layout.json","names-layout.json"):
    (root/name).write_text(json.dumps({"kernel_sha256":digest}))
   config=build_config(kernel,root)
   self.assertFalse(config["enable_compression"])
   self.assertEqual(config["kernel_image"],str(kernel.resolve()))
   (root/"owner-layout.json").write_text(json.dumps({"kernel_sha256":"wrong"}))
   with self.assertRaises(ValueError):build_config(kernel,root)

if __name__=="__main__":unittest.main()
