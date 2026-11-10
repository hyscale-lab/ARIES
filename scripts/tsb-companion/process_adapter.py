"""ARIES-facing subprocess adapter; no live guest-memory reads in the parent."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from capture_memory import CaptureFailure
from vmm_api import VmmRequestError

class ExternalCapture:
    def __init__(self,companion,api,socket_path,capture_id,vm_generation,epoch,output,
                 *,timeout_seconds=60):
        self.companion=Path(companion);self.api=api;self.socket_path=socket_path
        self.capture_id=capture_id;self.vm_generation=vm_generation;self.epoch=epoch
        self.output=Path(output);self.timeout_seconds=timeout_seconds
        self.process=None;self.log=None;self.sealed=False
    def start(self,*,bootstrap=None,reading=None,config=None,vm_id=None,consistency="dirty_validated",source_epoch=None,source_record_id=None):
        if self.process is not None:raise CaptureFailure("capture subprocess already started")
        self.consistency=consistency
        mode="bootstrap" if config is not None else "attribute"
        command=[sys.executable,str(self.companion/"capture.py"),mode,
                 "--socket",str(self.socket_path),"--capture-id",self.capture_id,
                 "--epoch",str(self.epoch),"--vm-generation",self.vm_generation,"--consistency",consistency,"--output",str(self.output)]
        if config is not None:command+=["--config",str(config)]
        if bootstrap is not None:command+=["--bootstrap",str(bootstrap)]
        if reading is not None:command+=["--reading",str(reading)]
        if vm_id is not None:command+=["--vm-id",vm_id]
        if source_epoch is not None:command+=["--source-epoch",str(source_epoch)]
        if source_record_id is not None:command+=["--source-record-id",source_record_id]
        self.output.parent.mkdir(parents=True,exist_ok=True)
        self.log=self.output.with_name(self.output.name+".log").open("xb")
        self.started_ns=time.monotonic_ns()
        self.process=subprocess.Popen(command,stdout=self.log,stderr=subprocess.STDOUT,start_new_session=True)
    def await_sealed(self):
        if self.process is None:raise CaptureFailure("capture subprocess not started")
        deadline=time.monotonic()+self.timeout_seconds
        while time.monotonic()<deadline:
            path=self.output/"capture-seal.json"
            if path.exists():
                begin=json.loads((self.output/"capture-begin.json").read_text())
                if begin["capture_id"]!=self.capture_id or begin["vm_generation"]!=self.vm_generation or begin.get("epoch")!=self.epoch:
                    self.abort("capture subprocess identity mismatch")
                    raise CaptureFailure("capture subprocess identity mismatch")
                try:seal=json.loads(path.read_text())
                except (ValueError,OSError):
                    self.abort("unreadable seal acknowledgement")
                    raise CaptureFailure("seal acknowledgement unreadable")
                expected="pending" if self.consistency=="dirty_validated" else "quiescence_only"
                if (seal.get("schema")!="aries.firecracker.metadata-capture.v1" or
                    seal.get("capture_id")!=self.capture_id or seal.get("vm_generation")!=self.vm_generation or
                    seal.get("epoch")!=self.epoch or seal.get("state")!="sealed" or
                    seal.get("consistency")!=expected or not isinstance(seal.get("capture_record_id"),str) or
                    not seal["capture_record_id"]):
                    self.abort("invalid seal acknowledgement")
                    raise CaptureFailure("seal acknowledgement identity/state invalid")
                self.sealed=True
                return seal
            if self.process.poll() is not None:
                pressure=self.output/"backpressure.json"
                if self.process.returncode==75 and pressure.exists():
                    receipt=json.loads(pressure.read_text())
                    error=VmmRequestError(receipt["endpoint"],receipt["operation"],receipt["http_status"],receipt["body"])
                    if error.is_safe_backpressure(self.vm_generation):
                        self.log.close()
                        raise error
                self.abort("capture process exited before seal")
                raise CaptureFailure("capture process exited before seal")
            time.sleep(.02)
        self.abort("capture live-read barrier timeout")
        raise CaptureFailure("capture live-read barrier timed out; VM remains caller-owned")
    def await_publication(self,timeout=60):
        code=self.process.wait(timeout=timeout)
        self.log.close()
        if code:raise CaptureFailure("capture publication failed with exit "+str(code))
        return {"returncode":code,"sealed":self.sealed,"finished_ns":time.monotonic_ns()}
    def abort(self,reason):
        # First reap all child readers; then revoke the VMM capture. Never resume here.
        if self.process is not None and self.process.poll() is None:
            os.killpg(self.process.pid,signal.SIGTERM)
            try:self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid,signal.SIGKILL)
                self.process.wait(timeout=2)
        if self.sealed:
            receipt=None
        else:
            info=self.api.call("/metadata-capture","info",capture_id=self.capture_id,
                               vm_generation=self.vm_generation)
            if (info.get("schema")!="aries.firecracker.metadata-capture.v1" or
                info.get("vm_generation")!=self.vm_generation or info.get("capture_id")!=self.capture_id or
                info.get("epoch")!=self.epoch):
                raise CaptureFailure("cannot resolve stopped capture state safely")
            if info.get("state")=="sealed":
                self.sealed=True;receipt=None
            elif info.get("state")=="aborted":
                receipt=info
            elif info.get("state")=="open":
                receipt=self.api.call("/metadata-capture","abort",capture_id=self.capture_id,
                                      vm_generation=self.vm_generation,reason=reason)
            else:raise CaptureFailure("unknown stopped capture state")
        if self.log is not None and not self.log.closed:self.log.close()
        return {"reason":reason,"reader_process_reaped":self.process is None or self.process.poll() is not None,
                "vmm_abort":receipt,"publication_canceled":self.sealed,
                "sealed_records_retained":self.sealed,"resume_performed":False}



    def discard_for_cleanup(self,reason):
        """Explicit failed-rotation recovery, distinct from publication cancellation."""
        if not reason:raise ValueError("cleanup discard requires an explicit reason")
        if self.process is not None and self.process.poll() is None:
            os.killpg(self.process.pid,signal.SIGTERM)
            try:self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid,signal.SIGKILL);self.process.wait(timeout=2)
        receipt=self.api.call("/metadata-capture","abort",capture_id=self.capture_id,
            vm_generation=self.vm_generation,reason="controller cleanup after failed rotation: "+reason)
        if (receipt.get("schema")!="aries.firecracker.metadata-capture.v1" or
            receipt.get("vm_generation")!=self.vm_generation or receipt.get("capture_id")!=self.capture_id or
            receipt.get("epoch")!=self.epoch or receipt.get("consistency")!="discarded"):
            raise CaptureFailure("cleanup discard acknowledgement invalid")
        if self.log is not None and not self.log.closed:self.log.close()
        return {"reader_process_reaped":True,"discarded":True,"consistency_valid":False,
                "original_sealed_evidence_preserved":self.sealed,"receipt":receipt,"resume_performed":False}
