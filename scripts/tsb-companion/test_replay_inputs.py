import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from replay import retained_reading

class ReplayInputs(unittest.TestCase):
    def test_unchanged_companion_parent_reading_and_legacy_step(self):
        with tempfile.TemporaryDirectory() as temporary:
            step=Path(temporary)/"access-dirty/captures/epoch-000003/attribution"
            step.mkdir(parents=True)
            value={"accessed_gpas":[4096],"unknown_gpas":{}}
            parent=step.parent/"reading.json";parent.write_text(json.dumps(value))
            before=parent.read_bytes()
            reading,path,directory=retained_reading(step)
            self.assertEqual(reading,value);self.assertEqual(path,parent)
            self.assertIsNone(directory);self.assertEqual(parent.read_bytes(),before)
            legacy={"accessed_gpas":[8192]}
            (step/"reading.json").write_text(json.dumps(legacy))
            self.assertEqual(retained_reading(step)[0],legacy)

    def test_compact_relocated_sibling_masks_ignore_worker_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary)/"relocated/access-dirty"
            step=root/"captures/epoch-000003/attribution";step.mkdir(parents=True)
            maskdir=root/"epoch-000003";maskdir.mkdir()
            masks={}
            for name,bits in {"access_observed":1,"access_unknown":2,"unknown_not-on-lru":2,
                              "access_idle":4,"kvm_dirty":4,"vmm_dirty":0,"combined_dirty":4}.items():
                raw=bits.to_bytes(8,"little");(maskdir/(name+".bin")).write_bytes(raw)
                masks[name]={"file":name+".bin","sha256":hashlib.sha256(raw).hexdigest(),"pages":bits.bit_count()}
            value={"artifact_directory":"/gone/worker/path","vm_generation":"g","source_epoch":3,
                   "regions":[{"guest_base":0,"bytes":12288,"masks":masks}]}
            reading_path=step.parent/"reading.json";reading_path.write_text(json.dumps(value))
            before=reading_path.read_bytes()
            reading,path,directory=retained_reading(step)
            self.assertEqual(directory,maskdir)
            self.assertEqual(reading["accessed_gpas"],[0])
            self.assertEqual(reading["unknown_gpas"],{"4096":"not-on-lru"})
            self.assertEqual(reading["dirty_gpas"],[8192])
            self.assertEqual(reading_path.read_bytes(),before)
            (maskdir/"kvm_dirty.bin").write_bytes(b"bad")
            with self.assertRaises(ValueError):retained_reading(step)

    def test_missing_retained_reading_fails_explicitly(self):
        with tempfile.TemporaryDirectory() as temporary:
            step=Path(temporary)/"attribution";step.mkdir()
            with self.assertRaisesRegex(FileNotFoundError,"no retained reading"):
                retained_reading(step)

if __name__=="__main__":unittest.main()
