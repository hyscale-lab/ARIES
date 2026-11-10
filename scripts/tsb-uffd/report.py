#!/usr/bin/env python3
"""Render retained UFFD pilot evidence; no new experiment or model call."""
import argparse
import html
import json
from pathlib import Path


def read(p):return json.loads(p.read_text())

def main():
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--root',type=Path,required=True);a=p.parse_args();r=a.root
 result=read(r/'result.json');ready=read(r/'ready.json');cleanup=read(r/'vm/cleanup.json');checks=read(r/'final-owned-cleanup.json')
 assert result['status']=='passed' and result['steps_completed']==4
 assert ready['machine_configuration']=={'vcpu_count':2,'mem_size_mib':4096}
 assert cleanup['status']=='confirmed' and read(r/'driver-exit.json')['exit_code']==0
 comparison=read(r/'comparison.json');compression=read(r/'compression.json');steps=[]
 for d in sorted(r.glob('step-*')):
  summary=read(d/'summary.json');resume=read(d/'resume.json');suspend=read(d/'suspend.json')
  ops=[read(f) for f in sorted(d.glob('operation-*.json'))]
  assert all(x['exit_code']==0 and not x['error'] for x in ops)
  assert summary['handler_summary']['failed'] is False
  steps.append({'step_id':summary['step_id'],'tool_first_fault_pages':len(summary['tool_only_first_fault_pages']),
   'restore_envelope_pages':len(summary['restore_to_tool_completion_first_fault_pages']),
   'phase_fault_events':summary['phase_fault_events'],
   'handler_cpu_seconds':summary['handler_summary']['cpu_ns']/1e9,
   'handler_lifetime_seconds':summary['handler_summary']['elapsed_ns']/1e9,
   'resume_seconds':(resume['end_monotonic_ns']-resume['begin_monotonic_ns'])/1e9,
   'suspend_seconds':(suspend['end_monotonic_ns']-suspend['begin_monotonic_ns'])/1e9,
   'operation_duration_seconds':sum(o['command_duration_ns'] for o in ops)/1e9,
   'operation_stdout':[o['stdout'] for o in ops],
   'summary':str((d/'summary.json').relative_to(r)),
   'fault_log':str((d/'faults.jsonl.gz').relative_to(r))})
 repeated=next(c for c in comparison if c['before']=='deterministic-2' and c['after']=='deterministic-3')
 repeated['same_hash_output']=steps[1]['operation_stdout']==steps[2]['operation_stdout']
 report={'title':'UFFD snapshot/restore pilot','status':'execution_and_cleanup_passed',
  'scope':'Four deterministic native VM steps; no paid requests. This pilot does not establish complete tool-exclusive read/write sets.',
  'machine_configuration':ready['machine_configuration'],'steps':steps,'elapsed_seconds':result['elapsed_seconds'],
  'identity_preserved':result['boot_disk_background_identity_preserved'],'repeated_read_comparison':repeated,
  'measurement_semantics':{'page_identity':'Snapshot byte offset / 4096, stable within the same VM memory layout; not host PFN.',
   'tool_only':'First missing-page faults observed during tool phase; excludes pages already populated during restore or validation even if the tool accesses them again.',
   'envelope':'First faults from cold restore through tool completion, including guest kernel, device, background and validation activity.',
   'snapshot':'Separate phase excluded from tool/envelope sets; full snapshot reads and materializes otherwise untouched pages.',
   'read_write':'First fault flags are retained; neither separate complete guest read/write sets nor repeated accesses are measured.'},
  'overhead_note':'Handler CPU and lifetime include full-snapshot page serving and JSON logging; not a controlled incremental slowdown measurement. Suspend includes snapshot writing, syncing and VMM teardown.',
  'compatibility':{'first_attempt':'../uffd-pilot-001/failure.json','reason':'Pinned vHive SDK emits container_snapshot_path, rejected by stock Firecracker 1.13.1.',
   'fix':'Worker-only SDK JSON-tag omission and shim rebuild, with exact retained-disk source equality guard; no daemon restart.',
   'provenance':'../uffd-compat/manifest.json','source_patch':'../uffd-compat/sdk.patch',
   'first_attempt_cleanup':'../uffd-pilot-001/manual-cleanup.json'},
  'cleanup':cleanup,'final_owned_cleanup':'final-owned-cleanup.json','handler_source_provenance':'executed-handler-source.json',
  'log_preservation':{'original_bytes':sum(x['original_bytes'] for x in compression),'compressed_bytes':sum(x['archive_bytes'] for x in compression),
   'decompressed_sha256_verified':all(x['decompressed_sha256_verified'] for x in compression),
   'recomputed_summaries_match':read(r/'compressed-summary-verification.json')['summary_from_compressed_logs_matches_original']}}
 (r/'pilot-report.json').write_text(json.dumps(report,indent=2)+'\n')
 esc=html.escape
 rows=''.join('<tr><td>'+esc(s['step_id'])+'</td><td>'+f"{s['tool_first_fault_pages']:,}"+'</td><td>'+f"{s['restore_envelope_pages']:,}"+'</td><td>'+f"{s['phase_fault_events'].get('snapshot',0):,}"+'</td><td>'+f"{s['handler_cpu_seconds']:.2f}"+'</td><td>'+f"{s['suspend_seconds']:.2f}"+'</td><td><a href="'+esc(s['summary'])+'">summary</a> · <a href="'+esc(s['fault_log'])+'">all faults (.gz)</a></td></tr>' for s in steps)
 repeated_tool=repeated['tool_only_first_fault_pages'];repeated_env=repeated['restore_to_tool_completion_first_fault_pages']
 body=f'''<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>UFFD snapshot/restore pilot</title>
<style>body{{font:16px/1.5 system-ui,sans-serif;max-width:1100px;margin:auto;padding:28px;color:#142333;background:#f5f7fa}}section{{background:white;padding:22px;margin:18px 0;border:1px solid #d7dee8;border-radius:10px}}table{{border-collapse:collapse;width:100%;font-size:14px}}td,th{{padding:9px;text-align:left;border-bottom:1px solid #ddd}}.scroll{{overflow:auto}}.note{{border-left:5px solid #b35c21}}a{{color:#145da0}}h1{{font-size:29px}}</style>
<h1>UFFD snapshot/restore pilot</h1><p>Four sequential steps · 2 vCPUs / 4096 MiB · no LLM requests · {result['elapsed_seconds']:.1f} seconds</p>
<section><h2>Execution and state preservation passed</h2><p>Each step restored the same guest, ran its operations, then created a fresh snapshot. Boot ID, disk marker and a live background PID survived every cycle. All commands exited zero. Final native cleanup was confirmed.</p><p><a href="result.json">Run result</a> · <a href="commands.json">Exact commands</a> · <a href="vm/cleanup.json">Native cleanup</a> · <a href="final-owned-cleanup.json">Final owned-resource inspection</a></p></section>
<section class="note"><h2>What these page sets mean</h2><p><strong>Tool-only faults are incomplete access sets.</strong> A page populated during restore does not fault again when the tool accesses it. The restore-to-tool envelope includes those earlier faults, together with kernel, device, background and identity-validation activity. Snapshot-materialization faults are recorded separately and excluded.</p><p>Page identity is the 4096-byte snapshot offset, compared within the same VM layout. This is first-touch evidence, not separate complete read/write sets or per-access tracing.</p></section>
<section><h2>Observed pages and cost</h2><div class="scroll"><table><thead><tr><th>Step</th><th>Tool-only pages</th><th>Restore envelope pages</th><th>Snapshot-only faults</th><th>Handler CPU s</th><th>Suspend s</th><th>Evidence</th></tr></thead><tbody>{rows}</tbody></table></div><p>The full snapshot materialized all 1,048,576 guest pages on every cycle. Handler CPU includes page serving and logging across all phases, mostly snapshot materialization. Suspend includes snapshot write/sync and VM teardown. These measurements do not isolate incremental slowdown against an uninstrumented baseline.</p></section>
<section><h2>Repeated read: steps 2 and 3</h2><p>Both commands read and hashed the same first 16 MiB and returned identical hashes. Tool-only overlap was {repeated_tool['intersection']:,} / {repeated_tool['union']:,} union pages (Jaccard {repeated_tool['jaccard']:.3f}); envelope overlap was {repeated_env['intersection']:,} / {repeated_env['union']:,} (Jaccard {repeated_env['jaccard']:.3f}). Different prefault timing prevents interpreting the tool-only change as genuine working-set turnover.</p><p><a href="comparison.json">All adjacent-step comparisons</a></p></section>
<section><h2>Initial failure and experimental compatibility fix</h2><p>Attempt 001 failed before the UFFD handshake because the pinned vHive SDK emitted <code>container_snapshot_path</code>, which stock Firecracker 1.13.1 rejects. A private SDK serialization patch and rebuilt worker shim omitted that field. The provider checks that the retained disk source is identical before UFFD restore. The original shim was preserved; the daemon was not restarted. This is a patched experimental stack.</p><p><a href="../uffd-pilot-001/failure.json">Original failure</a> · <a href="../uffd-pilot-001/manual-cleanup.json">Failed-attempt cleanup</a> · <a href="../uffd-compat/manifest.json">Patch/build provenance</a> · <a href="../uffd-compat/sdk.patch">Exact patch</a></p></section>
<section><h2>Preserved evidence</h2><p>All {report['log_preservation']['original_bytes']:,} bytes of fault JSONL are retained losslessly in gzip files totaling {report['log_preservation']['compressed_bytes']:,} bytes. Decompressed SHA-256 checks passed, and summaries recomputed from compressed logs exactly matched the originals. Working RAM backing snapshots were removed by native cleanup; they are not fault logs.</p><p><a href="compression.json">Compression checksums</a> · <a href="compressed-summary-verification.json">Summary verification</a> · <a href="executed-handler-source.json">Handler source hashes</a> · <a href="coordinator.log">Coordinator log</a> · <a href="pilot-report.json">Report data</a></p></section></html>'''
 methodology=(Path(__file__).resolve().parent.parent/'tsb-pages/methodology.html').read_text()
 body=body.replace('</html>',methodology+'</html>')
 (r/'pilot-report.html').write_text(body)
 for name in ['pilot-report.html','pilot-report.json']:(r/name).chmod(0o600)
 print(json.dumps({'status':'rendered','steps':len(steps),'html':str(r/'pilot-report.html'),'json':str(r/'pilot-report.json')}))

if __name__=='__main__':main()
