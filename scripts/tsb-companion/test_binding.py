import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import capture
from binding import bind_bootstrap,validate_live_inputs

class BindingTests(unittest.TestCase):
 def fixtures(self):
  bootstrap={"anchors":{"kernel_sha256":"hash"},"layout":{"kernel_sha256":"hash"}}
  bind_bootstrap(bootstrap,"g",[{"slot":0,"guest_base":0,"bytes":4096}],1)
  reading={"vm_generation":"g","vm_instance":"fixture","regions":bootstrap["capture_binding"]["regions"],
           "layout_identity":bootstrap["capture_binding"]["layout_identity"],"capture_epoch":3,
           "source_epoch":2,"source_record_id":"r","accessed_gpas":[0],"unknown_gpas":{}}
  return bootstrap,reading
 def test_wrong_live_identity_epoch_region_layout_rejected_before_api(self):
  bootstrap,reading=self.fixtures()
  for field,bad in (("vm_generation","wrong"),("capture_epoch",4),("source_epoch",1),
                    ("source_record_id","wrong"),("layout_identity","wrong"),
                    ("regions",[{"slot":0,"guest_base":4096,"bytes":4096}])):
   with self.subTest(field=field),tempfile.TemporaryDirectory() as directory:
    root=Path(directory);b=root/"bootstrap.json";r=root/"reading.json"
    b.write_text(json.dumps(bootstrap));changed=copy.deepcopy(reading);changed[field]=bad;r.write_text(json.dumps(changed))
    argv=["capture.py","attribute","--socket","/unused","--capture-id","c","--vm-generation","g",
          "--epoch","3","--source-epoch","2","--source-record-id","r","--bootstrap",str(b),
          "--reading",str(r),"--output",str(root/"out")]
    with patch("sys.argv",argv),patch.object(capture,"VmmAPI") as api:
     with self.assertRaises(ValueError):capture.main()
     api.assert_not_called()
 def test_manual_metadata_only_does_not_claim_access(self):
  bootstrap,reading=self.fixtures()
  reading.update(selection_scope="explicit_gpas",selected_gpas=[0],capture_epoch=None,accessed_gpas=[])
  validate_live_inputs(bootstrap,reading,generation="g",epoch=None,consistency="quiescence_only")
  reading["accessed_gpas"]=[0]
  with self.assertRaises(ValueError):
   validate_live_inputs(bootstrap,reading,generation="g",epoch=None,consistency="quiescence_only")

if __name__=="__main__":unittest.main()
