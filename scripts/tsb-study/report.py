#!/usr/bin/env python3
"""Render retained calibration and admission evidence; never fabricate Step data."""
import argparse
import collections
import hashlib
import json
from pathlib import Path

def load_evidence(root):
    trials, cases, whole, sources = [], [], [], []
    for p in sorted((root / "access-calibration").glob("primary-*.jsonl")):
        rows=[json.loads(line) for line in p.read_text().splitlines()]
        sources.append({"path":str(p.relative_to(root)),"sha256":hashlib.sha256(p.read_bytes()).hexdigest()})
        trials.append({"trial":p.stem,"events":len(rows),
            "gates":[r for r in rows if r.get("event")=="gate"],
            "failures":[r for r in rows if r.get("event")=="failure"]})
        for r in rows:
            if r.get("event") in ("case","extended_case"):
                reading=r["reading"]
                cases.append({"trial":p.stem,"case":r["case"],"wait_s":r["cadence_seconds"],
                    "recall":r.get("recall_including_unknown_as_miss"),"fp":r.get("false_positive_fraction"),
                    "unknown":len(reading.get("unknown_gpas",{})),
                    "unknown_reasons":dict(collections.Counter(reading.get("unknown_gpas",{}).values())),
                    "cpu_ms":r.get("observer_cpu_ns",0)/1e6,
                    "expected_pages":len(r["expected_gpas"]) if "expected_gpas" in r else None,
                    "observed_pages":len(reading["accessed_gpas"]),
                    "pass":r["target_gate_pass"]})
            if r.get("event")=="whole_resident_sample":
                whole.append({"trial":p.stem,**{k:v for k,v in r.items() if not isinstance(v,(dict,list))}})
    status_path=root/"study-status.json"
    status=json.loads(status_path.read_text()) if status_path.exists() else {"status":"in_progress","stages":[]}
    checks_path=root/"validation/status.txt"
    checks=checks_path.read_text().splitlines() if checks_path.exists() else []
    return {"schema_version":1,"scope":"observer calibration and benchmark admission, not benchmark H1/H2 results",
        "trials":trials,"cases":cases,"whole":whole,"status":status,"checks":checks,"sources":sources}

HTML=r"""<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ARIES sandbox memory: calibration and admission</title>
<style>
:root{font-family:system-ui,sans-serif;color:#172333;background:#f5f7fa}body{max-width:1150px;margin:auto;padding:28px}
h1{font-size:30px}h2{font-size:22px;margin-top:32px}.card{background:white;border:1px solid #d6dee7;border-radius:10px;padding:20px;margin:16px 0}
.notice{border-left:5px solid #a94b19}button,select{padding:8px 12px;margin-right:6px}table{border-collapse:collapse;width:100%;font-size:14px}
th,td{padding:9px;text-align:left;border-bottom:1px solid #e3e8ee}th{background:#edf2f8}.scroll{overflow:auto;max-height:480px}
.fail{color:#9c3017}.pass{color:#14633c}pre{white-space:pre-wrap;word-break:break-word}small{color:#526171}svg{width:100%;height:auto}.muted{color:#526171}
</style>
<header><button id="en">English</button><button id="ko">한국어</button>
<h1 data-en="Sandbox execution and memory-observer validity" data-ko="샌드박스 실행 및 메모리 관측기 유효성">Sandbox execution and memory-observer validity</h1>
<p data-en="Persistent Firecracker · fixed 2 vCPUs / 4096 MiB · dedicated amd205 worker" data-ko="지속 실행 Firecracker · 2 vCPU / 4096 MiB 고정 · 전용 amd205 워커">Persistent Firecracker · fixed 2 vCPUs / 4096 MiB · dedicated amd205 worker</p></header>
<section class="card notice"><h2 data-en="The access observer failed admission" data-ko="접근 페이지 관측기가 검증 기준을 통과하지 못했습니다">The access observer failed admission</h2>
<p data-en="Whole-sandbox H1 and H2 remain untested. Calibration found apparent read misses (identity artifacts unresolved), unknown page classes, and excessive observer overhead. No RSS, dirty, or fault counter is substituted for accessed-page sets." data-ko="전체 샌드박스 H1/H2는 아직 검증되지 않았습니다. 읽기 누락 가능성(페이지 식별 영향 미해결), 관측 불가 페이지, 높은 관측 비용이 확인되었습니다. RSS·dirty·fault 수치를 접근 페이지 집합으로 대체하지 않습니다.">Whole-sandbox H1 and H2 remain untested. Calibration found apparent read misses (identity artifacts unresolved), unknown page classes, and excessive observer overhead. No RSS, dirty, or fault counter is substituted for accessed-page sets.</p>
<p data-en="This is calibration evidence, not a completed 26-attempt benchmark study. Paid stages remain blocked by the declared validity gate." data-ko="이 결과는 캘리브레이션 증거이며, 26회 벤치마크 연구 완료 결과가 아닙니다. 유료 실행 단계는 사전 정의한 유효성 기준 때문에 진행되지 않았습니다.">This is calibration evidence, not a completed 26-attempt benchmark study. Paid stages remain blocked by the declared validity gate.</p></section>
<section class="card"><h2 data-en="Stage accounting" data-ko="단계별 실행 현황">Stage accounting</h2><div class="scroll"><table id="stages"></table></div><pre id="infra"></pre></section>
<section class="card"><h2 data-en="Calibration target recall" data-ko="캘리브레이션 대상 재현율">Calibration target recall</h2>
<p data-en="Filter by fresh-VM trial. Bars show the minimum observed recall for each target schedule; no-touch recall is undefined. These are controlled target sets, not agent Steps." data-ko="새 VM 시험별로 필터링할 수 있습니다. 막대는 접근 일정별 최소 재현율입니다. no-touch 재현율은 정의되지 않습니다. 이 결과는 에이전트 Step이 아닌 통제된 대상 집합입니다.">Filter by fresh-VM trial. Bars show the minimum observed recall for each target schedule; no-touch recall is undefined. These are controlled target sets, not agent Steps.</p>
<label for="trial">Trial / 시험 </label><select id="trial"></select><svg id="chart" viewBox="0 0 1000 350" role="img" aria-label="Minimum calibration recall by case"></svg>
<div class="scroll"><table id="cases"></table></div></section>
<section class="card"><h2 data-en="Full-resident scan overhead" data-ko="전체 상주 페이지 스캔 비용">Full-resident scan overhead</h2><div class="scroll"><table id="whole"></table></div>
<p data-en="Requested waiting interval is not achieved scan cadence. CPU cores are CPU time divided by the actual collection cycle. Endpoint coverage does not prove coverage of transient residency." data-ko="요청한 대기 간격은 실제 스캔 주기가 아닙니다. CPU 코어 수는 CPU 시간을 실제 수집 주기로 나눈 값입니다. 시작·종료 시점의 커버리지는 중간에만 상주한 페이지의 관측을 보장하지 않습니다.">Requested waiting interval is not achieved scan cadence. CPU cores are CPU time divided by the actual collection cycle. Endpoint coverage does not prove coverage of transient residency.</p></section>
<section class="card"><h2 data-en="Unavailable primary analyses" data-ko="생성할 수 없는 주요 분석">Unavailable primary analyses</h2>
<ul><li>Session → Turn → Step timelines: no admitted real-agent Sessions.</li><li>Per-Step W, M, W/M distributions: unavailable; host accounting is partial.</li><li>Page-by-Step maps, overlap matrices and turnover: not generated without validated complete Step page sets.</li><li>Retries, background jobs and compaction: native telemetry feasibility has explicit gaps; source patch is a follow-up unless runtime validation says otherwise.</li></ul>
<p data-en="Small or changing calibrated target sets do not establish H1/H2, reclaimability, eviction safety, prediction accuracy, or host-density gains." data-ko="작거나 변화하는 캘리브레이션 대상 집합만으로 H1/H2, 회수 가능성, 안전한 축출, 예측 정확도, 호스트 밀도 향상을 입증할 수 없습니다.">Small or changing calibrated target sets do not establish H1/H2, reclaimability, eviction safety, prediction accuracy, or host-density gains.</p></section>
<section class="card"><h2>Canonical methods and next experiment</h2><p>Identity is VM instance plus guest-physical slot captured from KVM registrations. Host PFNs are transient lookup keys. Only stable exclusive host-LRU base pages are currently tracked; compound/shared/non-LRU and changing mappings remain explicit unknowns. Guest page-idle fallback is absent. Guest physical identities were sampled at preparation/reuse, not after every command, so unexplained recall misses are apparent false negatives rather than proof of a specific KVM bug. The observer records reads and writes in known guest ranges; it does not establish foreground-tool causality. Full-resident samples count resident pages at scan endpoints, not all pages resident at any instant.</p>
<p>Next: reproduce read misses on a small known file-cache range while measuring host-LRU transitions and KVM accessed-bit/TLB behavior; evaluate a supported observer with explicit synchronization and batched metadata reads. Do not admit the full benchmark until this passes the original coverage/overhead gates. Complete native causal Step telemetry and non-overlapping host accounting separately.</p></section>
<section class="card"><h2 data-en="Checks and retained evidence" data-ko="검증 및 보존된 증거">Checks and retained evidence</h2><pre id="checks"></pre><details><summary>Raw input paths and SHA-256</summary><pre id="sources"></pre></details><details><summary>Trial gate records (includes failures)</summary><pre id="gates"></pre></details></section>
<script id="evidence" type="application/json">__EVIDENCE__</script>
<script>
const d=JSON.parse(document.getElementById('evidence').textContent),q=id=>document.getElementById(id);
function table(id,heads,rows){const t=q(id);t.replaceChildren();const h=t.insertRow();heads.forEach(x=>{const e=document.createElement('th');e.textContent=x;h.append(e)});rows.forEach(row=>{const r=t.insertRow();row.forEach(x=>{r.insertCell().textContent=x??'unknown'})})}
function fmt(x,n=3){return x==null?'undefined':Number(x).toFixed(n)}
table('stages',['Stage','Planned','Attempted','Completed','Failed','Blocked','Retries'],(d.status.stages||[]).map(s=>[s.stage,s.planned,s.attempted,s.completed,s.failed,s.blocked,s.retries]));
q('infra').textContent=JSON.stringify(d.status.infrastructure||{status:'pending verification'},null,2);
q('checks').textContent=d.checks.join('\n');q('sources').textContent=JSON.stringify(d.sources,null,2);q('gates').textContent=JSON.stringify(d.trials,null,2);
for(const name of ['all',...d.trials.map(t=>t.trial)]){const o=document.createElement('option');o.value=name;o.textContent=name;q('trial').append(o)}
table('whole',['Trial','Wait s','Resident MiB','Endpoint coverage','Unknown pages','CPU s','Actual cycle s','CPU cores','Observer peak MiB'],d.whole.map(r=>[r.trial,r.cadence_seconds,fmt(r.initial_resident_pages*4096/1048576,2),fmt(r.supported_resident_fraction_at_endpoints*100,3)+'%',r.unknown_pages_upper_at_endpoints,fmt(r.observer_cpu_ns/1e9),fmt(r.wall_ns/1e9),fmt(r.observer_cpu_cores_actual_cycle),fmt(r.observer_maxrss_after_kib/1024,1)]));
function draw(){const rows=d.cases.filter(r=>q('trial').value==='all'||r.trial===q('trial').value);
table('cases',['Trial','Case','Wait s','Recall','False positive','Unknown','CPU ms','Gate'],rows.map(r=>[r.trial,r.case,r.wait_s,fmt(r.recall),fmt(r.fp),r.unknown,fmt(r.cpu_ms,1),r.pass?'pass':'fail']));
const groups={};for(const r of rows){if(r.recall!==null)groups[r.case]=Math.min(groups[r.case]??1,r.recall)}
const svg=q('chart');svg.replaceChildren();const ns='http://www.w3.org/2000/svg';
function el(tag,attr,text){const e=document.createElementNS(ns,tag);for(const[k,v]of Object.entries(attr))e.setAttribute(k,v);if(text)e.textContent=text;svg.append(e)}
let i=0;for(const[name,value]of Object.entries(groups)){const y=15+i++*32;el('text',{x:0,y:y+16,'font-size':14},name);el('rect',{x:160,y,width:value*700,height:20,fill:value<.95?'#b8522d':'#377d74'});el('text',{x:870,y:y+16,'font-size':14},fmt(value*100,2)+'%')}
el('line',{x1:160+.95*700,x2:160+.95*700,y1:0,y2:330,stroke:'#172333','stroke-dasharray':'4 4'});}
q('trial').onchange=draw;draw();
function lang(code){document.documentElement.lang=code;document.querySelectorAll('[data-en]').forEach(e=>e.textContent=e.dataset[code]);}
q('en').onclick=()=>lang('en');q('ko').onclick=()=>lang('ko');
</script></html>"""

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root",type=Path,required=True)
    ap.add_argument("--output",type=Path,required=True)
    ap.add_argument("--stage",choices=["fix-git","five","twenty"])
    a=ap.parse_args();data=load_evidence(a.root)
    if not str(data["status"].get("status","")).startswith("measurement_gate_failed"):
        raise ValueError("This report renders the failed calibration/admission study only")
    if a.stage:
        data["status"]["stages"]=[s for s in data["status"]["stages"] if s["stage"]==a.stage]
        data["scope"] += "; stage "+a.stage+" remains unadmitted"
    content=json.dumps(data,ensure_ascii=False).replace("<","\\u003c")
    a.output.write_text(HTML.replace("__EVIDENCE__",content))
    a.output.chmod(0o600)
    a.output.with_suffix(".json").write_text(json.dumps(data,indent=2)+"\n")
    a.output.with_suffix(".json").chmod(0o600)
    print(a.output, len(data["cases"]), "calibration cases; no fabricated Step metrics")
if __name__=="__main__":main()
