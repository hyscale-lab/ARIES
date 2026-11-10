"""Capture Linux attribution through VMM APIs; publish and replay bytes externally.

This command never resumes the VM or rotates an epoch. The caller owns lifecycle.
Dirty-validated captures remain pending until that caller's next logical rotate.
"""
import argparse
import os
import gzip
import hashlib
import json
from pathlib import Path
from types import SimpleNamespace
from capture_memory import CaptureMemory
from decoder import GuestDecoder
from binding import bind_bootstrap,validate_live_inputs,region_identity
from vmm_api import VmmAPI, CaptureProvider, decode_page, VmmRequestError, export_rows

def write(path,value):
    path=Path(path);temporary=path.with_name(path.name+".tmp")
    with temporary.open("w") as stream:
        stream.write(json.dumps(value,indent=2)+"\n");stream.flush();os.fsync(stream.fileno())
    temporary.replace(path)

def decoder_from_bootstrap(bootstrap,memory,reading):
    d=GuestDecoder.__new__(GuestDecoder)
    d.pid=reading.get("vmm_pid",0);d.vm_id=reading["vm_instance"]
    d.slots=[];d.config=dict(bootstrap.get("attribution_config",{}))
    d.mem=memory;d.meta=bootstrap["layout"];d.l=d.meta["layout"]
    d.image=SimpleNamespace(symbols=d.meta["symbols"])
    d.a=bootstrap["anchors"];d.cache={};d.errors=[]
    d.info={}
    for line in d.a["vmcoreinfo"].splitlines():
        if "=" in line:
            k,v=line.split("=",1);d.info[k]=v
    return d

def publish_vmm_capture(provider,output):
    """Stream bounded exports; preserves prefetch/bootstrap reads absent decoder cache."""
    output=Path(output);output.mkdir(parents=True,exist_ok=True)
    gpas=[];seen=set();hashes=[];metadata=None
    with gzip.open(output/"captured-pages.bin.gz","wb",compresslevel=1) as stream:
        for rows,metadata in provider.export("pages"):
            for row in rows:
                data=decode_page(row)
                if row["gpa"] in seen:raise RuntimeError("duplicate exported GPA")
                seen.add(row["gpa"]);gpas.append(row["gpa"]);hashes.append(row["sha256"]);stream.write(data)
    write(output/"captured-page-index.json",{"page_bytes":4096,"gpas":gpas,"sha256":hashes,"metadata":metadata})
    for kind in ("read_mask","scan_log"):
        with gzip.open(output/(kind+".jsonl.gz"),"wt",compresslevel=1) as stream:
            for rows,metadata in provider.export(kind):
                for row in rows:stream.write(json.dumps(row)+"\n")
        write(output/(kind+"-metadata.json"),metadata)
    census_metadata=None
    with gzip.open(output/"resident-census.jsonl.gz","wt",compresslevel=1) as stream:
        for rows,census_metadata in export_rows(provider.api,"/memory-census",provider.census_record_id,
                "resident",max_rows=sum(r["bytes"]//4096 for r in provider.regions),
                expected_generation=provider.vm_generation):
            for row in rows:stream.write(json.dumps(row)+"\n")
    write(output/"resident-census-metadata.json",census_metadata)
    with gzip.open(output/"absent-ranges.jsonl.gz","wt",compresslevel=1) as stream:
        for rows,_ in export_rows(provider.api,"/memory-census",provider.census_record_id,"absent_ranges",
                max_rows=sum(r["bytes"]//4096 for r in provider.regions),expected_generation=provider.vm_generation):
            for row in rows:stream.write(json.dumps(row)+"\n")
    write(output/"capture-info.json",provider.info())
    artifacts={}
    for path in sorted(output.iterdir()):
        if not path.is_file():continue
        digest=hashlib.sha256()
        with path.open("rb") as stream:
            for block in iter(lambda:stream.read(1<<20),b""):digest.update(block)
            os.fsync(stream.fileno())
        artifacts[path.name]={"bytes":path.stat().st_size,"sha256":digest.hexdigest()}
    write(output/"export-manifest.json",{"record_id":provider.capture_record_id,
        "vm_generation":provider.vm_generation,"capture_id":provider.capture_id,
        "artifacts":artifacts,"complete":True})
    fd=os.open(output,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("mode",choices=("bootstrap","attribute"))
    ap.add_argument("--socket",required=True)
    ap.add_argument("--capture-id",required=True)
    ap.add_argument("--epoch",type=int)
    ap.add_argument("--consistency",choices=("dirty_validated","quiescence_only"),default="dirty_validated")
    ap.add_argument("--config",type=Path)
    ap.add_argument("--bootstrap",type=Path)
    ap.add_argument("--reading",type=Path)
    ap.add_argument("--vm-id")
    ap.add_argument("--vm-generation")
    ap.add_argument("--source-epoch",type=int)
    ap.add_argument("--source-record-id")
    ap.add_argument("--output",required=True,type=Path)
    ap.add_argument("--max-bytes",type=int,default=256<<20)
    ap.add_argument("--max-scan-bytes",type=int)
    args=ap.parse_args()
    if args.max_scan_bytes is None:args.max_scan_bytes=(1<<30) if args.mode=="bootstrap" else 0
    if args.max_bytes<=0 or args.max_scan_bytes<0:ap.error("capture budgets must be nonnegative with positive metadata bytes")
    if args.mode=="bootstrap":
        if not args.config:ap.error("bootstrap requires --config")
        config=json.loads(args.config.read_text())
        bootstrap=None;reading=None
    else:
        if not args.bootstrap or not args.reading:ap.error("attribute requires --bootstrap and --reading")
        bootstrap=json.loads(args.bootstrap.read_text());reading=json.loads(args.reading.read_text())
        config=dict(bootstrap.get("attribution_config",{}))
        if reading.get("selection_scope")=="explicit_gpas":
            reading.setdefault("accessed_gpas",[]);reading.setdefault("unknown_gpas",{})
        elif "accessed_gpas" not in reading:
            from compact import expand_reading
            reading=expand_reading(reading,artifact_directory=args.reading.parent)
    if args.mode=="attribute":
        validate_live_inputs(bootstrap,reading,generation=args.vm_generation,epoch=args.epoch,
            source_epoch=args.source_epoch,source_record_id=args.source_record_id,consistency=args.consistency)
    if config.get("enable_compression"):ap.error("payload compression is outside this capture milestone")
    if args.consistency=="dirty_validated" and not args.vm_generation:
        ap.error("dirty-validated capture requires --vm-generation from coordinator arm")
    args.output.mkdir(parents=True,exist_ok=False)
    api=VmmAPI(args.socket)
    try:
        provider=CaptureProvider(api,args.capture_id,mode="bootstrap" if args.mode=="bootstrap" else "attribution",
                                 consistency=args.consistency,epoch=args.epoch,vm_generation=args.vm_generation,max_bytes=args.max_bytes,
                                 max_scan_bytes=args.max_scan_bytes)
    except VmmRequestError as error:
        write(args.output/"begin-error.json",error.as_receipt())
        write(args.output/"api-calls.json",api.calls)
        if error.is_safe_backpressure(args.vm_generation):
            write(args.output/"backpressure.json",error.as_receipt())
            raise SystemExit(75)
        raise
    except BaseException as error:
        write(args.output/"begin-error.json",{"type":type(error).__name__,"message":str(error),"retry_safe":False})
        write(args.output/"api-calls.json",api.calls)
        raise
    memory=CaptureMemory(provider)
    write(args.output/"capture-begin.json",provider.begin)
    decode_completed=False
    try:
        if args.mode=="bootstrap":
            vm_id=args.vm_id or str(provider.vm_generation)
            d=GuestDecoder(0,vm_id,[],config,memory)
            bootstrap=d.bootstrap();decode_completed=True
            bind_bootstrap(bootstrap,provider.vm_generation,provider.regions,provider.epoch)
            write(args.output/"bootstrap.json",bootstrap)
            bundle=None
        else:
            if region_identity(provider.regions)!=region_identity(bootstrap["capture_binding"]["regions"]):
                raise ValueError("live VMM region map changed from validated bootstrap")
            d=decoder_from_bootstrap(bootstrap,memory,reading)
            bundle=d.capture_metadata(reading);decode_completed=True
            write(args.output/"reading.json",reading)
        seal=provider.seal()
        write(args.output/"capture-seal.json",seal)
        # Only bytes in the already-sealed provider may be used from here onward.
        if bundle is not None:d.publish_capture(bundle,args.output/"attribution")
        publish_vmm_capture(provider,args.output/"vmm-capture")
        write(args.output/"companion-metrics.json",{
            "read_requests":memory.read_requests,"requested_pages":memory.requested_pages,
            "cached_pages":len(memory.cache),"consumed_pages":len(memory.consumed_pages),
            "consistency_policy":args.consistency,
            "dirty_validation_pending":args.consistency=="dirty_validated",
            "live_discovery_includes_row_materialization":True,
            "api_calls":api.calls})
    except BaseException as error:
        if isinstance(error,VmmRequestError):write(args.output/"api-error.json",error.as_receipt())
        if provider.state=="open":
            try:
                write(args.output/"capture-abort.json",provider.abort())
                publish_vmm_capture(provider,args.output/"aborted-vmm-capture")
            except Exception as abort_error:
                write(args.output/"capture-abort-error.json",{"type":type(abort_error).__name__,"message":str(abort_error)})
        # A sealed capture has no live reader. Publication errors retain its records;
        # the controller may already have validated the epoch and resumed the VM.
        write(args.output/"failure.json",{"type":type(error).__name__,"message":str(error),
             "capture_state":provider.state,"capture_record_id":provider.capture_record_id,
             "live_reads_closed":provider.state in ("sealed","aborted"),
             "publication_failed":provider.state=="sealed",
             "attribution_decode_completed":decode_completed,
             "consistency_status":"consult finalized VMM receipt",
             "records_acknowledged":False,"retained_record_known":provider.capture_record_id is not None,"resume_performed":False})
        raise
    finally:
        write(args.output/"api-calls.json",api.calls)

if __name__=="__main__":main()
