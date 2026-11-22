"""Check source, final task state and measurement lineage against a validated TeX recording."""
import argparse
import hashlib
import json
from pathlib import Path


def verify(raw, receipt, variant, node, binary, hook):
    ref=json.loads(receipt.read_text())
    ev=[json.loads(line) for line in (raw/'events.jsonl').read_text().splitlines()]
    create,=[e for e in ev if e['event']=='create_start']
    assert ref['session_completed'] and ref['all_observed_tool_exits_match'] and ref['task']=='overfull-hbox'
    assert ev[-1]['event']=='removed' and create['trial_id']==ref['trial_id'] and create['job_id']==ref['job_id']
    for k in ('image','trajectory_sha256','rollout_sha256','memory_mib','storage_mib','vcpus'):
        assert ref[k]==create[k],k
    observed=[e for e in ev if e['event']=='tool_reply']
    assert [(e['step_id'],e['exit_code']) for e in observed]==[(e['step_id'],e['exit_code']) for e in ref['tools']]
    final,=[e for e in ev if e['event']=='final_task_state']
    expected,=[e for e in ref['source_setup_and_final_state'] if e['event']=='final_task_state']
    assert final['checks']==expected['checks']
    digest=hashlib.sha256(binary.read_bytes()).hexdigest()
    assert digest==ref['adapter_binary_sha256']
    revisions=dict(aries_cli=ref['adapter_revision'],aries_cli_sha256=digest,aries_hook=hook,
                   runtime={'v1':'bad3e9dd65c5db211ed5768bff834d4ef69806e0',
                            'v2':'4fb117126ba45480bc34c4573fd498dd66c59dc4',
                            'v3':'4873139f7e26c2c0761f336960b49784fa028aab',
                            'v4':'4873139f7e26c2c0761f336960b49784fa028aab'}[variant],
                   stock_firecracker='1.18.0',guest_kernel='7483450dec64b4f923037de31d8c28298a7c3359',
                   root_ext4_sha256='292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f')
    return dict(run_id=raw.name,workload=ref['task'],variant=variant,node=node,source_trial=ref['trial_id'],
                job_id=ref['job_id'],status='validated',snapshot_id=None,snapshot_mode='native-file' if variant in ('v3','v4') else 'file',
                revisions=revisions,source_validation_receipt=receipt.name,final_checks=final['checks'],
                memory_mib=create['memory_mib'],storage_mib=create['storage_mib'],vcpus=create['vcpus'],
                fidelity=dict(expected_exits=[e['expected_exit_code'] for e in observed],
                              output_comparisons=[e['output_comparison'] for e in observed]))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('raw',type=Path);p.add_argument('receipt',type=Path)
    p.add_argument('variant',choices=('v1','v2','v3','v4'));p.add_argument('node');p.add_argument('binary',type=Path);p.add_argument('hook')
    a=p.parse_args()
    print(json.dumps(verify(a.raw,a.receipt,a.variant,a.node,a.binary,a.hook),indent=2))
