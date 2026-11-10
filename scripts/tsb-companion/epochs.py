"""Optional epoch coordinator client. Standalone metric users need not import it."""
import gzip
import hashlib
import json
import os
from pathlib import Path
from capture_memory import CaptureFailure
from binding import region_identity
from vmm_api import export_rows, ack_record, VmmRequestError

class EpochCoordinator:
    def __init__(self,api,metrics):
        selected=list(metrics)
        if not selected or len(set(selected))!=len(selected) or not set(selected)<={"dirty","idle","census","metadata"}:
            raise ValueError("unique supported metric names required")
        self.api=api;self.metrics=selected;self.generation=None;self.epoch=None;self.regions=None;self.failed=False
    def _check(self,result):
        if result.get("schema")!="aries.firecracker.trace-epochs.v1" or not isinstance(result.get("vm_generation"),str) or not result["vm_generation"]:
            raise CaptureFailure("coordinator schema mismatch")
        if self.generation is not None and result.get("vm_generation")!=self.generation:
            raise CaptureFailure("coordinator VM generation changed")
    def arm(self,phase):
        if self.generation is not None or self.failed:raise CaptureFailure("coordinator cannot be rearmed")
        try:
            result=self.api.call("/trace-epochs","arm",epoch=1,phase=phase,metrics=self.metrics)
            self._check(result)
            if result.get("active_epoch")!=1 or result.get("epoch")!=1:
                raise CaptureFailure("coordinator initial epoch mismatch")
            try:
                regions=region_identity(result["regions"])
                if not regions or any(r["page_count"]!=raw["page_count"] for r,raw in zip(regions,sorted(result["regions"],key=lambda r:r["guest_base"]))):
                    raise ValueError("invalid page counts")
            except (ValueError,KeyError,TypeError):
                raise CaptureFailure("coordinator authoritative RAM regions invalid")
            if result.get("baseline_discarded") is not True or set(result.get("selected_metrics",[]))!=set(self.metrics):
                raise CaptureFailure("coordinator initial baseline/metric selection mismatch")
        except VmmRequestError as error:
            if not error.is_safe_backpressure(self.generation):self.failed=True
            raise
        except Exception:
            self.failed=True;raise
        self.generation=result["vm_generation"];self.epoch=1;self.regions=result["regions"]
        return result
    def rotate(self,phase=None,*,close=False):
        if self.epoch is None or self.failed:raise CaptureFailure("no trustworthy active epoch")
        next_epoch=None if close else self.epoch+1
        try:
            result=self.api.call("/trace-epochs","rotate",vm_generation=self.generation,epoch=self.epoch,
                                 next_epoch=next_epoch,phase=phase)
            self._check(result)
            try:
                if region_identity(result["regions"])!=region_identity(self.regions):
                    raise CaptureFailure("coordinator authoritative RAM regions changed")
            except (ValueError,KeyError,TypeError):
                raise CaptureFailure("coordinator authoritative RAM region response invalid")
            if result.get("epoch")!=self.epoch or result.get("next_epoch")!=next_epoch or result.get("active_epoch")!=next_epoch:
                raise CaptureFailure("coordinator rotation mismatch; no implicit recovery reset")
        except VmmRequestError as error:
            if not error.is_safe_backpressure(self.generation):self.failed=True
            raise
        except Exception:
            self.failed=True;raise
        self.epoch=next_epoch
        return result
    def publish(self,result,output):
        """Retain bounded immutable export chunks, then acknowledge owned record."""
        self._check(result)
        record_id=result["epoch_record_id"];output=Path(output);output.mkdir(parents=True,exist_ok=False)
        receipts={};metadata={}
        for kind,field in (("access","access_record_id"),("dirty","dirty_record_id"),("resident","census_record_id"),("absent_ranges","census_record_id")):
            if result.get(field) is None:continue
            if result[field]!=record_id:raise CaptureFailure("coordinator dataset alias differs")
            path=output/(kind+".jsonl.gz");count=0
            with gzip.open(path,"wt",compresslevel=1) as stream:
                for rows,meta in export_rows(self.api,"/trace-epochs",record_id,kind,
                        max_rows=sum(r["page_count"] for r in self.regions),expected_generation=self.generation):
                    metadata[kind]=meta
                    for row in rows:stream.write(json.dumps(row)+"\n");count+=1
            digest=hashlib.sha256()
            with path.open("rb") as stream:
                for block in iter(lambda:stream.read(1<<20),b""):digest.update(block)
                os.fsync(stream.fileno())
            receipts[kind]={"file":path.name,"rows":count,"bytes":path.stat().st_size,"sha256":digest.hexdigest()}
        manifest={"response":result,"datasets":receipts,"metadata":metadata,"vm_generation":self.generation,
                  "record_id":record_id,"complete":True,"guest_payload_reads":False}
        temporary=output/"manifest.json.tmp"
        with temporary.open("w") as stream:
            json.dump(manifest,stream,indent=2);stream.write("\n");stream.flush();os.fsync(stream.fileno())
        temporary.replace(output/"manifest.json")
        fd=os.open(output,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
        ack_record(self.api,"/trace-epochs",record_id,self.generation)
        return manifest
