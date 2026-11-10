#!/usr/bin/env python3
"""Compare observed per-step page sets; retain unknowns and boundary meanings."""
import argparse
import collections
import hashlib
import html
import json
import os
import time
from pathlib import Path


def elapsed_ms(begin, end):
    if not isinstance(begin, (int,float)) or not isinstance(end, (int,float)) or end < begin:
        return None
    return (end-begin)/1e6


def compare_sets(before, after):
    """Directional A -> B comparison. An empty union has no defined Jaccard."""
    a,b=set(before),set(after)
    n=len(a|b)
    return {'intersection_pages':len(a&b),'union_pages':n,'new_pages':len(b-a),
            'lost_pages':len(a-b),'jaccard':len(a&b)/n if n else None,
            'new_fraction_of_union':len(b-a)/n if n else None,
            'lost_fraction_of_union':len(a-b)/n if n else None}


# Bounded dense GPA bitmaps accelerate exact pair cardinalities. Sparse or
# unusual addresses keep set semantics without allocating by a huge GPA.
_MAX_BITMAP_BYTES = 8 * 1024 * 1024
_POSTPROCESSOR_VERSION = "gpa-bitmaps-exact-v1"


class _PreparedPages:
    __slots__ = ("bitmap", "bytes", "sparse", "count", "page_bytes")

    def __init__(self, values, page_bytes):
        self.page_bytes = page_bytes
        self.count = len(values)  # normalized inputs are already unique
        self.bitmap = self.bytes = self.sparse = None
        maximum = max(values, default=0)
        width = maximum // page_bytes // 8 + 1
        dense = (maximum >= 0 and width <= _MAX_BITMAP_BYTES
                 and width <= max(4096, self.count * 16)
                 and all(isinstance(g, int) and g >= 0 and g % page_bytes == 0 for g in values))
        if dense:
            bits = bytearray(width)
            for gpa in values:
                page = gpa // page_bytes
                bits[page >> 3] |= 1 << (page & 7)
            self.bytes = bytes(bits)
            self.bitmap = int.from_bytes(self.bytes, "little")
        else:
            self.sparse = frozenset(values)
            self.count = len(self.sparse)

    def intersection(self, other):
        if self.sparse is None and other.sparse is None:
            return (self.bitmap & other.bitmap).bit_count()
        if self.sparse is not None and other.sparse is not None:
            return len(self.sparse & other.sparse)
        sparse, dense = (self, other) if self.sparse is not None else (other, self)
        total = 0
        for gpa in sparse.sparse:
            if gpa < 0 or gpa % dense.page_bytes:
                continue
            page = gpa // dense.page_bytes
            byte = page >> 3
            if byte < len(dense.bytes) and dense.bytes[byte] & (1 << (page & 7)):
                total += 1
        return total


def _metrics(before_count, after_count, intersection):
    union = before_count + after_count - intersection
    new = after_count - intersection
    lost = before_count - intersection
    return {
        "intersection_pages": intersection, "union_pages": union,
        "new_pages": new, "lost_pages": lost,
        "jaccard": intersection / union if union else None,
        "new_fraction_of_union": new / union if union else None,
        "lost_fraction_of_union": lost / union if union else None,
    }


def normalize(row, index, artifact_root=None):
    from compact import expand_reading
    raw=row['reading']
    directory=None
    if artifact_root is not None and raw.get('vm_generation') and 'accessed_gpas' not in raw:
        epoch=raw.get('source_epoch')
        if not isinstance(epoch,int) or epoch<1:raise ValueError('VMM compact source epoch required')
        directory=Path(artifact_root)/'access-dirty'/('epoch-%06d'%epoch)
    r=expand_reading(raw,artifact_directory=directory);group=row.get('event')=='group_end';native=group or row.get('event')=='step_end'
    segmented=r.get('schema')=='aries.tsb.tool-pages.segmented.v2'
    llm_batch=r.get('schema') in ('aries.tsb.tool-pages.llm-batch.v1','aries.vmm-tracer.compact.v1')
    page_bytes=r.get('page_bytes')
    if not isinstance(page_bytes,int) or page_bytes<=0:raise ValueError('positive page_bytes required')
    touched=r.get('accessed_gpas')
    if not isinstance(touched,list) or any(not isinstance(g,int) or g<0 or g%page_bytes for g in touched):
        raise ValueError('accessed_gpas must be aligned nonnegative integers')
    if len(set(touched))!=len(touched):raise ValueError('duplicate accessed GPA')
    unknown=r.get('unknown_gpas',{})
    if not isinstance(unknown,dict):raise ValueError('unknown_gpas must be mapping')
    idle=set(r.get('idle_gpas',[]))
    touched_set=set(touched);unknown_set={int(g) for g in unknown}
    counted=touched_set|unknown_set|set(r.get('dirty_gpas',[]))
    if touched_set&(idle|unknown_set):raise ValueError('accessed/idle/unknown classifications overlap')
    result=row.get('result',{});metadata=row.get('metadata',{})
    return {'index':index,'session_id':row.get('session_id'),'turn_id':row.get('turn_id'),
            'step_id':row.get('group_id') if group else row.get('step_id',row.get('tool_call_id')),
            'group_id':row.get('group_id'),
            'batch_id':row.get('batch_id'),
            'llm_step_id':row.get('llm_step_id'),
            'expected_tool_call_ids':row.get('expected_tool_call_ids',[]),
            'native_assistant_timestamp_ms':row.get('native_assistant_timestamp_ms'),
            'native_assistant_message_id':row.get('native_assistant_message_id'),
            'member_tool_call_ids':row.get('member_tool_call_ids',[]),
            'group_members':row.get('group_members',[]),
            'concurrency_peak':row.get('concurrency_peak'),
            'record_kind':('llm_reasoning_step' if llm_batch else 'native_busy_period_group' if group else 'native_hook_interval_partial' if segmented else 'native_llm_tool_step') if native else 'mechanism_window_not_native_step',
            'reading_schema':r.get('schema'),
            'vm_generation':r.get('vm_generation'),
            'source_epoch':r.get('source_epoch'),
            'source_record_id':r.get('source_record_id'),
            'capture_epoch':r.get('capture_epoch'),
            'coverage_complete':r.get('coverage_complete'),
            'measurement_valid_scope':r.get('measurement_valid_scope',row.get('measurement_valid_scope')),
            'coverage_uncertainty_reasons':r.get('coverage_uncertainty_reasons',[]),
            'segment_ids':r.get('segment_ids',[]),
            'overlapping_tool_call_ids':r.get('overlapping_tool_call_ids',[]),
            'transition_gaps':r.get('transition_gaps',[]),
            'transition_gap_envelope_ms':r.get('transition_gap_envelope_ns',0)/1e6,
            'segment_arm_total_ms':r.get('segment_arm_total_ns',0)/1e6 if segmented else None,
            'segment_scan_total_ms':r.get('segment_scan_total_ns',0)/1e6 if segmented else None,
            'gap_counting_policy':r.get('gap_counting_policy'),
            'tool_name':row.get('tool_name'),'vm_instance':r.get('vm_instance'),
            'tool_result_is_error':metadata.get('is_error') if native else (result.get('exit_code') != 0 if 'exit_code' in result else None),
            'measurement_valid':row.get('measurement_valid',True),
            'page_bytes':page_bytes,'accessed_pages':len(touched),'accessed_bytes':len(touched)*page_bytes,
            'idle_pages':len(idle),'unknown_pages':len(unknown),
            'counted_touched_gpas':sorted(counted),
            'counted_touched_pages':len(counted),
            'counted_touched_bytes':len(counted)*page_bytes,
            'unknown_counts':dict(collections.Counter(unknown.values())),
            'dirty_pages':len(r.get('dirty_gpas',[])),
            'counted_policy':r.get('counted_policy','accessed union unknown'),
            'not_resident_at_arm_gpa_ranges':r.get('not_resident_at_arm_gpa_ranges'),
            'not_resident_at_end_gpa_ranges':r.get('not_resident_at_end_gpa_ranges'),
            'tool_interval_ms':elapsed_ms(result.get('begin_monotonic_ns'),result.get('end_monotonic_ns')),
            'arm_ms':elapsed_ms(r.get('arm_begin_ns'),r.get('arm_end_ns')),
            'scan_ms':elapsed_ms(r.get('scan_begin_ns'),r.get('scan_end_ns')),
            'observer_cpu_ms':(r['arm_cpu_ns']+r['scan_cpu_ns'])/1e6 if all(isinstance(r.get(k),(int,float)) for k in ('arm_cpu_ns','scan_cpu_ns')) else None,
            'observation_envelope_ms':elapsed_ms(r.get('arm_begin_ns'),r.get('scan_end_ns')),
            'operation_count':len(row['operations']) if isinstance(row.get('operations'),list) else metadata.get('operation_count'),
            'accessed_gpas':touched,'unknown_gpas':unknown,
            'caveats':r.get('caveats',[])}


def summarize(rows, mode='auto', artifact_root=None):
    if mode=='auto':
        mode=('group' if any(r.get('event')=='group_end' for r in rows) else
              'segmented' if any(r.get('reading',{}).get('schema')=='aries.tsb.tool-pages.segmented.v2' for r in rows) else 'legacy')
    allowed=('group_end',) if mode=='group' else (None,'step_end')
    selected=[r for r in rows if 'reading' in r and r.get('event') in allowed]
    steps=[normalize(r,i,artifact_root=artifact_root) for i,r in enumerate(selected)]
    kinds={s['record_kind'] for s in steps}
    unit=(('llm_reasoning_step' if not kinds or kinds=={'llm_reasoning_step'} else
           'busy_period_group' if kinds=={'native_busy_period_group'} else 'mixed_group_schemas')
          if mode=='group' else 'native_hook_interval' if mode=='segmented' else 'native_tool_call_window')
    prepared_observed=[_PreparedPages(s["accessed_gpas"],s["page_bytes"]) for s in steps]
    prepared_counted=[prepared_observed[i] if not s["unknown_pages"] and not s.get("dirty_pages") else _PreparedPages(s["counted_touched_gpas"],s["page_bytes"]) for i,s in enumerate(steps)]
    intersections={}
    def metrics(a,b,prepared,kind):
        i,j=a["index"],b["index"]
        lo,hi=sorted((i,j))
        key=(kind,lo,hi)
        if key not in intersections:
            intersections[key]=prepared[lo].count if lo==hi else prepared[lo].intersection(prepared[hi])
        return _metrics(prepared[i].count,prepared[j].count,intersections[key])
    pairs=[];observed_pairs=[]
    for a in steps:
        for b in steps:
            reason=None
            if not a['measurement_valid'] or not b['measurement_valid']:reason='invalid measurement window'
            elif not a['vm_instance'] or a['vm_instance']!=b['vm_instance']:reason='different or missing VM identity'
            elif a['page_bytes']!=b['page_bytes']:reason='different page granularity'
            elif a['record_kind']!=b['record_kind']:reason='different boundary kinds'
            p={'from_index':a['index'],'to_index':b['index'],'comparable':reason is None,'reason':reason,
               'shared_segment_ids':sorted(set(a['segment_ids'])&set(b['segment_ids']))}
            p.update(metrics(a,b,prepared_observed,'observed') if reason is None else
                     {k:None for k in ('intersection_pages','union_pages','new_pages','lost_pages','jaccard','new_fraction_of_union','lost_fraction_of_union')})
            observed_pairs.append(dict(p))
            if reason is None:p.update(metrics(a,b,prepared_counted,'counted'))
            pairs.append(p)
    return {'schema':'aries.tsb.step-page-comparison.v3','mode':mode,
            'comparison_unit':unit,
            'steps':steps,'pairs':pairs,
            'observed_only_pairs':observed_pairs,
            'counting_policy':('Effective footprint = raw observed access union unknown-as-touched union conservative KVM/VMM dirty candidates; raw access classifications unchanged.' if any(s.get('dirty_pages') for s in steps) else 'All recorded unknown GPAs are counted as touched, by user-requested assumption; not a reclassification of raw evidence.'),
            'hierarchy':{'session':'one end-to-end task','turn':'one user-harness interaction',
                         'step':'one LLM tool-request batch through all expected results ready for the LLM' if unit=='llm_reasoning_step' else 'historical observation unit, see comparison_unit',
                         'native_call':'one native call within the LLM step; admission can be absent for an immediate error',
                         'bridge_operation':'a sandbox operation implementing a native call'},
            'coverage':{'complete':False,'scope':'partial temporal observations; no complete or exclusively causal footprint claim',
                        'comparison_unit':unit,
                        'native_member_calls':sum(len(s['member_tool_call_ids']) for s in steps) if mode=='group' else len(steps),
                        'steps_with_overlap':sum(bool(s['overlapping_tool_call_ids']) for s in steps),
                        'steps_with_transition_gaps':sum(bool(s['transition_gaps']) for s in steps)},
            'meaning':['PARTIAL temporal coverage: measurement_valid for segmented windows certifies protocol pairing only, not complete page coverage.',
                       'The primary LLM-step method declares all native tool IDs from one complete assistant response before harness preparation, then collects after every expected result. Sequential gaps and parallel/expanded operations share one bitmap arm; native scheduling is unchanged.',
                       'Historical busy-period records retain their original schema and granularity; they are not reinterpreted as full LLM reasoning steps.',
                       'Experimental segmented mode unions shared segments across before-hook/result-persistence intervals; overlapping windows are not independent and intermediate resets can miss accesses.',
                       'Native result-persistence delivery may occur after execution completion; hook intervals are not exact individual execution durations.',
                       'In experimental segmented mode, interior bitmap read/reset gaps can lose accesses by continuing calls. Conservative gap envelopes and potentially affected RAM ranges are retained; these unlocalized gaps are not expanded into counted per-page unknowns.',
                       'In experimental segmented mode, accessed pages union across segments; observed access takes priority over an unknown classification elsewhere. Idle means idle in every recorded segment, not idle throughout the full native interval.',
                       'Primary matrix counts observed touched UNION all unknown pages as touched, as requested. This is an analysis assumption, not proof every unknown was accessed.',
                       'Jaccard = intersection / union of the two step sets; 0.80 means 80% of their combined distinct pages are shared.',
                       'Failed tool calls remain in the record; pages observed during a call with no sandbox operations are not proven command accesses.',
                       'Unknown is not idle; endpoint absence does not establish absence throughout the window.',
                       'The interval brackets a native step, not periodic sampling cadence; segmented coverage can have explicit interior reset gaps.',
                       'Mechanism windows are not evidence of a native harness session or general multi-operation step.',
                       'GPA membership is compared only inside the same VM identity; GPA reuse may change page contents or ownership.',
                       'Guest kernel, background work and host/VMM activity can contribute; overlap does not prove causal tool attribution.',
                       'New/lost denote observed set differences, not allocation, eviction, or guaranteed access differences.',
                       'Empty-union Jaccard and fractions are null. Different VM identities are never matched.']}


def render(summary):
    e=lambda v:html.escape(str(v if v is not None else 'unknown'))
    n=len(summary['steps']);matrix=[]
    for a in range(n):
        cells=['<th>'+str(a+1)+'</th>']
        for b in range(n):
            p=summary['pairs'][a*n+b];v=p['jaccard']
            label='—' if v is None else f'{v:.3f}'
            tip=p['reason'] or f"intersection={p['intersection_pages']}; new={p['new_pages']}; lost={p['lost_pages']}; union={p['union_pages']}"
            shade=95 if v is None else 95-55*v
            cells.append(f'<td style="background:hsl(180 35% {shade}%)" title="{e(tip)}">{label}</td>')
        matrix.append('<tr>'+''.join(cells)+'</tr>')
    records=[]
    for s in summary['steps']:
        vals=[s['index']+1,s['session_id'],s['turn_id'],s['step_id'],s['record_kind'],s['tool_result_is_error'],s['counted_touched_pages'],round(s['counted_touched_bytes']/1048576,3),s['accessed_pages'],s['unknown_pages'],len(s['segment_ids']),', '.join(s['member_tool_call_ids'] or s['overlapping_tool_call_ids']) or 'none',len(s['transition_gaps']),s['transition_gap_envelope_ms'],'PARTIAL' if s['coverage_complete'] is False else 'not certified',s['tool_interval_ms'],s['arm_ms'],s['scan_ms'],s['measurement_valid']]
        records.append('<tr>'+''.join('<td>'+e(round(v,3) if isinstance(v,float) else v)+'</td>' for v in vals)+'</tr>')
    headers=['#','Session','Turn','Step / window ID','Boundary','Tool error','Counted touched pages','MiB (including unknown)','Observed touched','Unknown included','Segments','Member / overlapping call IDs','Interior gaps','Gap envelope ms','Coverage','Hook / group interval ms','Initial arm ms','Final scan ms','Protocol paired']
    baseline=dict(summary);baseline['pairs']=summary.get('observed_only_pairs',summary['pairs'])
    baseline_rows=[]
    for a in range(n):
        baseline_rows.append('<tr><th>'+str(a+1)+'</th>'+''.join('<td>'+('—' if p['jaccard'] is None else f"{p['jaccard']:.3f}")+'</td>' for p in baseline['pairs'][a*n:(a+1)*n])+'</tr>')
    baseline_html='<h2>Original matrix: observed touched only</h2><div class="scroll"><table><tr><th>From / to</th>'+''.join('<th>'+str(i+1)+'</th>' for i in range(n))+'</tr>'+''.join(baseline_rows)+'</table></div>'
    methodology=Path(__file__).with_name('methodology.html').read_text()
    coverage=summary['coverage']
    if summary['comparison_unit']=='llm_reasoning_step':
        notice=('<p><strong>ONE MATRIX ROW PER LLM REASONING STEP.</strong> '+str(coverage['native_member_calls'])+' native tool calls belong to '+str(n)+' LLM tool-request batches. Each bitmap arm precedes native harness preparation, covers all requested calls and their expanded operations regardless of sequential/parallel scheduling, and ends when every expected native result is ready for the LLM. There are no resets inside an LLM step. Per-call page sets were not measured.</p><p>Batch registration and expected-result completion do not certify complete or exclusively causal page coverage. Guest-kernel, background and host/VMM activity is included.</p>')
    elif summary['mode']=='group':
        notice='<p><strong>HISTORICAL BUSY-PERIOD PROTOTYPE.</strong> These readings are the original overlapping-call envelopes, not full LLM reasoning-step batches. Membership and original schemas are retained; they are not relabeled as the primary LLM-step method.</p>'
    elif summary['mode']=='segmented':
        notice=('<p><strong>EXPERIMENTAL SEGMENTED HOOK INTERVALS.</strong> Result delivery can lag execution completion; interior resets can miss accesses by continuing calls. '+str(coverage['steps_with_overlap'])+' hook windows overlap other calls; '+str(coverage['steps_with_transition_gaps'])+' contain conservative bitmap read/reset gap envelopes. Unlocalized possible accesses in those gaps are not added to the counted page set. Shared observations are not independent per-call footprints.</p>')
    else:
        notice='<p><strong>LEGACY SINGLE-WINDOW OBSERVATIONS.</strong> These records use the original arm/collect procedure. They are not reinterpreted as experimental segmented windows. Finite scans, endpoint checks and KVM translation limits prevent a complete causal footprint claim.</p>'
    warning='<aside style="border:2px solid #b36b00;background:#fff6df;padding:16px"><strong>PARTIAL TEMPORAL COVERAGE</strong>'+notice+'</aside>'
    title=('LLM reasoning-step page comparison' if summary['comparison_unit']=='llm_reasoning_step' else
           'Historical busy-period page comparison' if summary['mode']=='group' else 'Per-step observed page comparison')
    unit_description=('Session → user/harness turn → LLM reasoning step → native calls and bridge operations. Each row and matrix index is one LLM tool-request batch; native IDs/times remain separate.' if summary['comparison_unit']=='llm_reasoning_step' else
                      'Historical observation units retain their original schema and membership; see comparison_unit.')
    return '<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>'+title+'</title><style>body{font:15px system-ui;margin:32px;color:#17232b}table{border-collapse:collapse}td,th{padding:8px;border:1px solid #ccd6db;text-align:left}.scroll{overflow:auto}li{margin:8px 0}h1{font-size:26px}</style><h1>'+title+'</h1>'+warning+'<p>'+unit_description+'</p><ul>'+''.join('<li>'+e(v)+'</li>' for v in summary['meaning'])+'</ul>'+methodology+'<h2>Observed intervals</h2><div class="scroll"><table><tr>'+''.join('<th>'+h+'</th>' for h in headers)+'</tr>'+''.join(records)+'</table></div><h2>Jaccard matrix: all unknown pages counted as touched</h2><p>Row → column. Hover for intersection, new, lost and union page counts. — means undefined or incomparable.</p><div class="scroll"><table><tr><th>From / to</th>'+''.join('<th>'+str(i+1)+'</th>' for i in range(n))+'</tr>'+''.join(matrix)+'</table></div>'+baseline_html+'<p>Raw GPA lists, unknown reasons and directional pair metrics are retained in the sibling JSON summary and source JSONL.</p></html>'


def _file_sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream,"sha256").hexdigest()


def _save_postprocessor(path, record):
    temporary=path.with_name("."+path.name+"."+str(os.getpid())+".tmp")
    with temporary.open("w") as stream:
        json.dump(record,stream,indent=2);stream.write("\n")
        stream.flush();os.fsync(stream.fileno())
    os.replace(temporary,path)


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--input',type=Path,required=True)
    ap.add_argument('--artifact-root',type=Path,help='retained observer directory; defaults to input parent for relocatable VMM epoch masks')
    ap.add_argument('--output',type=Path,required=True,help='summary JSON; sibling .html also written')
    ap.add_argument('--mode',choices=('auto','group','segmented','legacy'),default='auto',help='group compares group_end records only; segmented compares native hook windows')
    a=ap.parse_args();os.umask(0o077)
    # Capture the implementation before any input parsing/building, so a later
    # source deployment cannot be attributed to this already-running process.
    began=time.monotonic()
    postprocessor={
        "schema":"aries.tsb.comparison-postprocessor.v1",
        "version":_POSTPROCESSOR_VERSION,
        "source_path":str(Path(__file__).resolve()),
        "source_sha256":hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        "input_path":str(a.input),"json_path":str(a.output),
        "html_path":str(a.output.with_suffix(".html")),"status":"started",
    }
    a.output.parent.mkdir(parents=True,exist_ok=True)
    sidecar=a.output.parent/"comparison-postprocessor.json"
    _save_postprocessor(sidecar,postprocessor)
    try:
        data=a.input.read_bytes()
        summary=summarize([json.loads(line) for line in data.splitlines() if line.strip()],a.mode,artifact_root=a.artifact_root or a.input.parent)
        summary['source']={'path':str(a.input),'sha256':hashlib.sha256(data).hexdigest()}
        a.output.write_text(json.dumps(summary,indent=2)+'\n');a.output.chmod(0o600)
        page=a.output.with_suffix('.html');page.write_text(render(summary));page.chmod(0o600)
        json_sha,html_sha=_file_sha256(a.output),_file_sha256(page)
        postprocessor.update(status="completed",input_sha256=summary["source"]["sha256"],
                             elapsed_seconds=time.monotonic()-began,records=len(summary["steps"]),
                             json_sha256=json_sha,html_sha256=html_sha)
    except BaseException as error:
        postprocessor.update(status="failed",error_type=type(error).__name__,
                             elapsed_seconds=time.monotonic()-began)
        _save_postprocessor(sidecar,postprocessor)
        raise
    _save_postprocessor(sidecar,postprocessor)
    print(json.dumps({'records':len(summary['steps']),'json':str(a.output),'html':str(page)}))

if __name__=='__main__':main()
