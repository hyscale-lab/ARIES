#!/usr/bin/env python3
"""Compare reopened CAD files to the recorded geometry and retain ZIP member evidence."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys

CASES = {
    'freecad-platform-drawing': ('21341911317b13a30199d76f8b4d9994c61103c78079300c8e0b65c2989188ce',
                                'call_efey6AGvfOd0BWwyRG9Mf19E', ['/app/part.FCStd'], 4),
    'freecad-spring-clip': ('aa4c558a4791da575f3636c7e1ffefa33b79b168586fff68b849753c2bbe47ce',
                           'call_KdMPujbUMcwfEUXvaPaACeli', ['/app/answer_base.FCStd', '/app/answer_edit.FCStd'], 2),
}

# ZIP timestamps and document metadata may vary between otherwise identical CAD saves.
ARCHIVE_INSPECTION = """
import hashlib, json, sys, zipfile, xml.etree.ElementTree as ET
for path in sys.argv[1:]:
    members = []
    with zipfile.ZipFile(path) as archive:
        for entry in archive.infolist():
            payload = archive.read(entry)
            members.append(dict(name=entry.filename, size=len(payload), zip_time=entry.date_time,
                                sha256=hashlib.sha256(payload).hexdigest()))
        xml = ET.fromstring(archive.read('Document.xml'))
        dates = {p.get('name'): p[0].get('value') for p in xml.findall('./Properties/Property')
                 if p.get('name') in {'CreationDate', 'LastModifiedDate', 'Uid'} and len(p)}
    print(json.dumps(dict(file=path, document_metadata=dates, members=members)))
"""


def main(fixture, run, cli, sandbox, reference):
    task = json.loads((fixture/'metadata.json').read_text())['task']
    trajectory_sha, geometry_call, outputs, count = CASES[task]
    source = (fixture/'trajectory').read_bytes()
    assert hashlib.sha256(source).hexdigest() == trajectory_sha
    step, call = next((s, c) for s in json.loads(source)['steps'] for c in s.get('tool_calls', [])
                      if c['tool_call_id'] == geometry_call)
    recorded = next(r['content'] for r in step['observation']['results'] if r['source_call_id'] == geometry_call)
    expected = recorded.split('\nOutput:\n', 1)[1].strip()
    program = call['arguments']['cmd'].split("/opt/conda/bin/python - <<'PY'\n", 1)[1].split('\nPY', 1)[0]
    if task == 'freecad-platform-drawing':
        # The suffix removes transient crops and prints a directory listing with wall-clock mtimes.
        program, marker, _ = program.partition("\nfor filename in ('crop.png','detail.png','section.png'):")
        assert marker and "\n-rw-" in expected
        expected = expected.split('\n-rw-', 1)[0]
    with (run/'events.jsonl').open() as stream:
        events = [json.loads(line) for line in stream]
    replies = [e for e in events if e['event'] == 'tool_reply']
    assert len(replies) == 15 and all(e['exit_code'] == e['expected_exit_code'] for e in replies)
    images = [e for e in replies if e['operation'] == 'ViewImage']
    assert len(images) == count and all(e['exit_code'] == 0 and e['source_output_sha256'] == e['stdout_sha256'] for e in images)
    assert any(e['source_call_id'] == geometry_call and e['exit_code'] == 0 for e in replies)
    assert all(e['source_call_id'] != 'call_VobtTkN3dXgCtrlbKgWPcFxw' for e in replies)
    def guest(script, *args):
        cmd = [cli, 'exec', sandbox, 'runc', 'exec', '--cwd', '/app', 'zb-workload',
               'env', 'QT_QPA_PLATFORM=offscreen', '/opt/conda/bin/python', '-c', script, *args]
        return subprocess.check_output(cmd, text=True).strip()
    geometry = guest(program)
    assert geometry == expected, (expected, geometry)
    archives = [json.loads(line) for line in guest(ARCHIVE_INSPECTION, *outputs).splitlines()]
    assert [a['file'] for a in archives] == outputs and all(len(a['members']) >= 2 for a in archives)
    actual, name = (run/'output.sha256').read_text().split()
    assert name == outputs[0]
    receipt = dict(status='passed', mode='source_geometry_and_image_replies', reference_sha256=reference,
                   actual_sha256=actual, byte_identical=actual == reference, trajectory_sha256=trajectory_sha,
                   source_geometry_call=geometry_call, reopened_geometry_output=geometry,
                   image_replies={e['source_call_id']: e['stdout_sha256'] for e in images}, archives=archives)
    with (run/'cad-validation.json').open('x') as f:
        json.dump(receipt, f, indent=2)
        f.write('\n')


if __name__ == '__main__':
    main(Path(sys.argv[1]), Path(sys.argv[2]), *sys.argv[3:])
