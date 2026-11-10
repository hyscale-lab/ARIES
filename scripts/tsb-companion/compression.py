"""Metadata-only sampling from an immutable VMM-owned census."""
import hashlib
class ResidentSample:
 def __init__(self,decoder,reading):
  self.d=decoder;self.reading=reading;self.selected=[];self.counts={}
  self.seed=int.from_bytes(hashlib.sha256((decoder.vm_id+':'+str(reading.get('tool_call_id'))+':resident-v2').encode()).digest()[:8],'little')
 def census(self):
  record=self.d.mem.resident_sample(self.seed,int(self.d.config.get('resident_sample_pages',1024)))
  self.selected=record['selection'];self.counts=record['resident_population'];self.design=record['sample_design']
  if len({s['gpa'] for s in self.selected})!=len(self.selected) or len({s['host_pfn'] for s in self.selected})!=len(self.selected):
   raise RuntimeError('invalid VMM resident sample identities')
  return {s['gpa'] for s in self.selected}
 def collect(self,*args):
  raise RuntimeError('payload compression is outside the capture companion')
