"""Bounded HTTP/Unix-socket bindings for the independently selectable VMM APIs."""
import base64
import hashlib
import http.client
import json
import random
import socket
import time
from capture_memory import CaptureFailure, PAGE, MAX_BATCH

class VmmRequestError(CaptureFailure):
    """Structured failure retained verbatim; retry classification is explicit."""
    def __init__(self,endpoint,operation,status,body):
        self.endpoint=endpoint;self.operation=operation;self.status=status;self.body=body
        super().__init__("VMM "+endpoint+" "+operation+" HTTP "+str(status))
    def is_safe_backpressure(self,expected_generation):
        if not isinstance(self.body,dict) or not isinstance(self.body.get("error"),dict):return False
        error=self.body["error"]
        return (self.status==400 and isinstance(expected_generation,str) and bool(expected_generation) and
                self.body.get("schema")=="aries.firecracker."+self.endpoint.strip("/")+".v1" and
                self.body.get("vm_generation")==expected_generation and
                error.get("code")=="backpressure" and error.get("mutation_performed") is False and
                error.get("reset_performed") is False and error.get("poisoned") is False)
    def as_receipt(self):
        return {"endpoint":self.endpoint,"operation":self.operation,"http_status":self.status,"body":self.body}

class UnixConnection(http.client.HTTPConnection):
    def __init__(self,path,timeout):
        super().__init__("localhost",timeout=timeout);self.path=str(path)
    def connect(self):
        self.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout);self.sock.connect(self.path)

class VmmAPI:
    def __init__(self,socket_path,timeout=10,max_response_bytes=16<<20):
        self.socket_path=str(socket_path);self.timeout=timeout
        self.max_response_bytes=max_response_bytes;self.calls=[]
    def call(self,endpoint,operation,**fields):
        conn=UnixConnection(self.socket_path,self.timeout)
        request={"operation":operation,**fields}
        receipt={"endpoint":endpoint,"operation":operation,"begin_ns":time.monotonic_ns(),"outcome":"failed"}
        try:
            conn.request("PUT",endpoint,json.dumps(request),{"Content-Type":"application/json"})
            response=conn.getresponse()
            receipt["http_status"]=response.status
            raw=response.read(self.max_response_bytes+1)
            receipt["response_bytes"]=len(raw)
            if len(raw)>self.max_response_bytes:raise CaptureFailure("VMM response exceeds client bound")
            if response.status!=200:
                try:body=json.loads(raw)
                except (ValueError,UnicodeError):body={"unparsed_response":raw.decode(errors="replace")}
                receipt["error_response"]=body
                raise VmmRequestError(endpoint,operation,response.status,body)
            result=json.loads(raw)
            if not isinstance(result,dict):raise CaptureFailure("invalid VMM response type")
            receipt["outcome"]="returned"
            return result
        except (OSError,http.client.HTTPException,ValueError) as error:
            raise CaptureFailure("capture transport failed; no implicit retry/resume") from error
        finally:
            conn.close();receipt["end_ns"]=time.monotonic_ns();self.calls.append(receipt)

def decode_page(row):
    try:data=base64.b64decode(row["data_b64"],validate=True)
    except (ValueError,TypeError) as error:raise CaptureFailure("invalid captured byte encoding") from error
    if len(data)!=PAGE or hashlib.sha256(data).hexdigest()!=row["sha256"]:
        raise CaptureFailure("captured page size/hash mismatch")
    return data

def export_rows(api,endpoint,record_id,kind,limit=4096,max_rows=None,*,expected_generation):
    offset=0;seen_metadata=None;expected_total=None
    while True:
        response=api.call(endpoint,"export",record_id=record_id,kind=kind,offset=offset,limit=limit)
        if response.get("schema")!="aries.firecracker."+endpoint.strip("/")+".v1" or response.get("vm_generation")!=expected_generation:
            raise CaptureFailure("export generation/schema mismatch")
        if response.get("record_id")!=record_id or response.get("kind")!=kind or response.get("offset")!=offset:
            raise CaptureFailure("export identity/cursor mismatch")
        rows=response["rows"];next_offset=response["next_offset"];total=response["total_rows"]
        if not isinstance(total,int) or total<0:raise CaptureFailure("invalid export total")
        if expected_total is None:expected_total=total
        elif total!=expected_total:raise CaptureFailure("immutable export total changed")
        if not isinstance(rows,list) or len(rows)>limit or next_offset!=offset+len(rows):
            raise CaptureFailure("export pagination malformed")
        if max_rows is not None and total>max_rows:raise CaptureFailure("export exceeds declared total bound")
        if next_offset>total or bool(response["complete"])!=(next_offset==total):
            raise CaptureFailure("export completion/total mismatch")
        metadata=response.get("metadata",{})
        if seen_metadata is None:seen_metadata=metadata
        elif metadata!=seen_metadata:raise CaptureFailure("immutable export metadata changed")
        yield rows,metadata
        if response["complete"]:break
        if next_offset<=offset:raise CaptureFailure("export made no progress")
        offset=next_offset

class CaptureProvider:
    def __init__(self,api,capture_id,*,mode="attribution",consistency="dirty_validated",
                 epoch=None,vm_generation=None,max_bytes=256<<20,max_scan_bytes=None):
        if consistency not in ("dirty_validated","quiescence_only"):
            raise ValueError("unsupported consistency policy")
        if mode not in ("bootstrap","attribution"):raise ValueError("unsupported capture mode")
        if max_scan_bytes is None:max_scan_bytes=(1<<30) if mode=="bootstrap" else 0
        if max_bytes<=0 or max_scan_bytes<0:raise ValueError("invalid capture budgets")
        if consistency=="dirty_validated" and vm_generation is None:
            raise ValueError("dirty-validated capture requires the coordinator VM generation")
        self.api=api;self.capture_id=capture_id;self.consistency=consistency
        fields={"capture_id":capture_id,"mode":mode,"consistency":consistency,
                "max_bytes":max_bytes,"max_scan_bytes":max_scan_bytes}
        if epoch is not None:fields["epoch"]=epoch
        if vm_generation is not None:fields["vm_generation"]=vm_generation
        self.begin=api.call("/metadata-capture","begin",**fields)
        if self.begin.get("schema")!="aries.firecracker.metadata-capture.v1" or self.begin.get("capture_id")!=capture_id or self.begin.get("state")!="open":
            raise CaptureFailure("invalid capture begin identity/state")
        self.vm_generation=self.begin["vm_generation"];self.epoch=self.begin.get("epoch")
        if vm_generation is not None and self.vm_generation!=vm_generation:raise CaptureFailure("capture begin VM generation mismatch")
        if epoch is not None and epoch!=self.epoch:raise CaptureFailure("capture epoch mismatch")
        self.regions=self.begin["regions"];self.budgets=self.begin["budgets"]
        if self.budgets["max_bytes"]>max_bytes or self.budgets["max_scan_bytes"]>max_scan_bytes:
            raise CaptureFailure("VMM accepted larger budgets than requested")
        self.census_record_id=self.begin["census_record_id"]
        self.pages={};self.read_receipts=[];self.scan_receipts=[];self.state="open"
        self.capture_record_id=None
    @property
    def remaining_pages(self):return max(0,self.budgets["max_bytes"]//PAGE-len(self.pages))
    def _call(self,operation,**fields):
        result=self.api.call("/metadata-capture",operation,capture_id=self.capture_id,
                             vm_generation=self.vm_generation,**fields)
        if result.get("schema")!="aries.firecracker.metadata-capture.v1" or result.get("vm_generation")!=self.vm_generation or result.get("capture_id")!=self.capture_id or "epoch" not in result or result["epoch"]!=self.epoch:
            raise CaptureFailure("capture response generation/schema mismatch")
        return result
    def read_pages(self,gpas):
        if self.state!="open":raise CaptureFailure("live reads forbidden after seal/abort")
        if len(gpas)>MAX_BATCH or len(set(gpas))!=len(gpas):raise CaptureFailure("invalid read batch")
        missing=set(gpas)-self.pages.keys()
        if len(missing)>self.remaining_pages:raise CaptureFailure("retained metadata budget exhausted")
        response=self._call("read_pages",gpas=list(gpas))
        rows=response["pages"]
        if len(rows)!=len(gpas) or {r["gpa"] for r in rows}!=set(gpas):
            raise CaptureFailure("read response GPA identity mismatch")
        self.read_receipts.extend({k:v for k,v in row.items() if k!="data_b64"} for row in rows)
        result={}
        for row in rows:
            # Error receipt remains available even if a later row in the batch fails.
            if row.get("status")!="ok":raise CaptureFailure("VMM page read status "+str(row.get("status")))
            data=decode_page(row);g=row["gpa"]
            if g in self.pages and data!=self.pages[g]:raise CaptureFailure("first-copy bytes changed")
            self.pages[g]=data;result[g]=data
        return result
    def find(self,needle,max_gpa=1<<30):
        if self.state!="open":raise CaptureFailure("scan forbidden after seal")
        if not isinstance(needle,bytes) or not needle or len(needle)>256:
            raise ValueError("bounded nonempty search pattern required")
        cursor=0;matches=[];scanned=0
        while True:
            response=self._call("scan",needle_b64=base64.b64encode(needle).decode(),
                start_gpa=0,end_gpa=max_gpa,cursor=cursor,max_scan_bytes=1<<20,resident_only=True)
            self.scan_receipts.append(response)
            scanned+=response["scanned_bytes"]
            if not 0<=response["scanned_bytes"]<=1<<20 or scanned>self.budgets["max_scan_bytes"]:
                raise CaptureFailure("scan exceeded accepted byte budget")
            for match in response["matches"]:
                g=match["gpa"]+match.get("offset",0) if isinstance(match,dict) else match
                if not 0<=g<max_gpa:raise CaptureFailure("scan match outside search range")
                matches.append(g)
            next_cursor=response["next_cursor"]
            if response["complete"]:break
            if not cursor<next_cursor<=max_gpa:raise CaptureFailure("scan cursor failed progress/bounds")
            cursor=next_cursor
        return sorted(set(matches))
    def resident_sample(self,seed,requested):
        if requested<0:raise ValueError("negative sample size")
        unique={};metadata=None;resident_count=0;masked=0
        bound=sum(r["bytes"]//PAGE for r in self.regions)
        for rows,metadata in export_rows(self.api,"/memory-census",self.census_record_id,"resident",max_rows=bound,expected_generation=self.vm_generation):
            for row in rows:
                resident_count+=1;pfn=row["pfn"]
                if pfn:unique.setdefault(pfn,row)
                else:masked+=1
        counts=metadata["resident_population"]
        if counts["resident_gpa_pages"]!=resident_count or counts["unique_nonzero_host_pfns"]!=len(unique) or counts["zero_pfn_pages"]!=masked:
            raise CaptureFailure("census identity/count mismatch")
        n=min(requested,len(unique));indices=set(random.Random(seed).sample(range(len(unique)),n))
        selected=[{"gpa":row["gpa"],"host_pfn":pfn,"host_kpageflags":row["host_kpageflags"],
                   "host_kpagecount":row["host_kpagecount"]} for i,(pfn,row) in enumerate(unique.items()) if i in indices]
        if any(not isinstance(r[k],int) or r[k]<0 for r in selected for k in ("host_pfn","host_kpageflags","host_kpagecount")):
            raise CaptureFailure("selected census metadata unavailable")
        return {"selection":selected,"resident_population":{
            "resident_gpa_pages":resident_count,"unique_nonzero_host_pfns":len(unique),"zero_pfn_pages":masked,
            "pre_capture_vmm_smaps_rollup":{"Rss_kib":counts.get("vmm_rss_kib"),"Pss_kib":counts.get("vmm_pss_kib")},
            "census_ns":metadata["inventory_end_ns"]-metadata["inventory_begin_ns"]},
            "sample_design":{"unit":"one representative GPA per distinct nonzero resident host PFN",
                            "population_pages":len(unique),"sample_pages":n,"requested_pages":requested,
                            "seed":seed,"rule":"uniform without replacement"}}
    def seal(self):
        if self.state!="open":raise CaptureFailure("capture already closed")
        result=self._call("seal")
        expected="pending" if self.consistency=="dirty_validated" else "quiescence_only"
        if result.get("state")!="sealed" or result.get("consistency")!=expected or not isinstance(result.get("capture_record_id"),str) or not result["capture_record_id"]:
            raise CaptureFailure("seal did not confirm closed reads and expected consistency state")
        self.state="sealed";self.capture_record_id=result["capture_record_id"]
        return result
    def info(self):return self._call("info")
    def abort(self,reason="companion capture failure or cancellation"):
        result=self._call("abort",reason=reason);self.state="aborted"
        self.capture_record_id=result.get("capture_record_id");return result
    def export(self,kind):
        if self.state not in ("sealed","aborted") or not self.capture_record_id:
            raise CaptureFailure("only sealed/aborted capture can be exported")
        limit=MAX_BATCH if kind=="pages" else 4096
        bound=self.budgets["max_bytes"]//PAGE if kind=="pages" else sum(r["bytes"]//PAGE for r in self.regions)
        return export_rows(self.api,"/metadata-capture",self.capture_record_id,kind,limit=limit,max_rows=bound,expected_generation=self.vm_generation)



def finalize_capture(api,begin,seal):
    """Resolve a verdict without changing any epoch or guest state."""
    info=api.call("/metadata-capture","info",capture_id=begin["capture_id"],
                  vm_generation=begin["vm_generation"])
    if (info.get("schema")!="aries.firecracker.metadata-capture.v1" or
        info.get("vm_generation")!=begin["vm_generation"] or
        info.get("capture_id")!=begin["capture_id"] or
        "epoch" not in info or info["epoch"]!=begin.get("epoch")):
        raise CaptureFailure("finalized capture response identity mismatch")
    status=info.get("consistency")
    if status not in ("valid","invalid","quiescence_only","discarded"):
        raise CaptureFailure("capture consistency has no final verdict")
    old=seal["capture_record_id"];final=info["capture_record_id"]
    if status in ("valid","invalid"):
        if final==old or info.get("sealed_record_id")!=old:
            raise CaptureFailure("finalized capture does not reference retained sealed record")
    elif status=="quiescence_only":
        if seal.get("consistency")!="quiescence_only" or final!=old:
            raise CaptureFailure("unexpected quiescence-only final record")
    # Invalid/discarded results are returned explicitly, never reclassified as valid.
    return info


def ack_record(api,endpoint,record_id,vm_generation):
    """Caller must retain and fsync exported artifacts before releasing a record."""
    response=api.call(endpoint,"ack",record_id=record_id,vm_generation=vm_generation)
    if response.get("schema")!="aries.firecracker."+endpoint.strip("/")+".v1" or response.get("vm_generation")!=vm_generation:
        raise CaptureFailure("ack response identity mismatch")
    return response
