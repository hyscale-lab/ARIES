"""Verify recovered compiler-field ordering and offset validation without a kernel build."""
import contextlib
import hashlib
import io
import json
from pathlib import Path
import runpy
import struct
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT=Path(__file__).resolve().parent
PROFILE=ROOT/'layouts/linux-5.10.239'


class LayoutToolsTests(unittest.TestCase):
    def test_regenerated_probe_preserves_the_recovered_field_order(self):
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);kernel=root/'kernel-source';(kernel/'kernel').mkdir(parents=True)
            output=root/'generated'
            subprocess.run([sys.executable,str(ROOT/'layout_generate.py'),'--output-dir',str(output),
                            '--kernel-source',str(kernel)],check=True)
            expected=(PROFILE/'layout_offsets.c').read_bytes()
            self.assertEqual((output/'layout_offsets.c').read_bytes(),expected)
            self.assertEqual((kernel/'kernel/layout_offsets.c').read_bytes(),expected)
            self.assertEqual(json.loads((output/'offset-names.json').read_text()),
                             json.loads((PROFILE/'offset-names.json').read_text()))

    def test_compiled_field_vector_decodes_to_the_pinned_base_layout(self):
        # The vector is fixture input, not a native compiler/kernel validation.
        source=json.loads((PROFILE/'base-layout.json').read_text())
        names=json.loads((PROFILE/'offset-names.json').read_text())
        values=[source['layout'][kind][field] for kind,field in (name.split('.',1) for name in names)]
        symbols='\n'.join(f'{value:016x} 00000001 D {name}' for name,value in source['symbols'].items())
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);offsets=root/'offsets.bin';kernel=root/'synthetic.elf';output=root/'layout.json'
            offsets.write_bytes(struct.pack('<'+'Q'*len(values),*values));kernel.write_bytes(b'\x7fELF-offset-vector-fixture')
            argv=['layout_decode.py','--offsets',str(offsets),'--kernel-image',str(kernel),'--output',str(output)]
            with patch('sys.argv',argv),patch('subprocess.check_output',return_value=symbols),contextlib.redirect_stdout(io.StringIO()):
                runpy.run_path(str(ROOT/'layout_decode.py'),run_name='__main__')
            result=json.loads(output.read_text())
            self.assertEqual(result['layout'],source['layout'])
            self.assertEqual(result['kernel_sha256'],hashlib.sha256(kernel.read_bytes()).hexdigest())
            self.assertTrue(all(result['checks'].values()))

    def test_validation_fails_closed_for_mismatch_or_no_comparable_fields(self):
        cases=[('SIZE(page)=64\nOFFSET(page.lru)=8\nNUMBER(PG_dirty)=3\n',True),
               ('SIZE(page)=65\n',False),('OSRELEASE=5.10.239\n',False)]
        with tempfile.TemporaryDirectory() as temporary:
            root=Path(temporary);anchors=root/'anchors.json';output=root/'validation.json'
            for vmcoreinfo,passes in cases:
                with self.subTest(vmcoreinfo=vmcoreinfo):
                    anchors.write_text(json.dumps({'vmcoreinfo':vmcoreinfo}))
                    result=subprocess.run([sys.executable,str(ROOT/'layout_validate.py'),'--anchors',str(anchors),
                                           '--output',str(output)],capture_output=True,text=True)
                    self.assertEqual(result.returncode,0 if passes else 1)
                    report=json.loads(output.read_text());self.assertEqual(report['all_match'],passes)
                    if passes:self.assertEqual(len(report['checks']),3)

    def test_layout_and_probe_checksums_match_preserved_sources(self):
        for line in (PROFILE/'SHA256SUMS').read_text().splitlines():
            digest,name=line.split()
            self.assertEqual(hashlib.sha256((PROFILE/name).read_bytes()).hexdigest(),digest,name)


if __name__=='__main__':unittest.main()
