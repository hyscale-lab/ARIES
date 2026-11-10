"""Uniform resident-host-PFN sampling and per-page codec measurements."""
import os,struct,time,random,hashlib,json,zlib,collections
from pathlib import Path
class ResidentSample:
 def __init__(self,decoder,reading):
  self.d=decoder;self.reading=reading;self.selected=[];self.counts={}
  self.seed=int.from_bytes(hashlib.sha256((decoder.vm_id+':'+str(reading.get('tool_call_id'))+':resident-v2').encode()).digest()[:8],'little')
 def census(self):
  start=time.monotonic_ns();unique={};resident_gpas=0;masked=0
  for slot in self.d.slots:
   n=slot['memory_size']//4096
   for j in range(0,n,8192):
    k=min(8192,n-j);data=os.pread(self.d.mem.pm,k*8,(slot['userspace_addr']//4096+j)*8)
    if len(data)!=k*8:raise ValueError('short resident census')
    for i,(entry,) in enumerate(struct.iter_unpack('<Q',data)):
     if entry&(1<<63):
      resident_gpas+=1;pfn=entry&((1<<55)-1)
      if pfn:unique.setdefault(pfn,slot['guest_phys_addr']+(j+i)*4096)
      else:masked+=1
  requested=int(self.d.config.get('resident_sample_pages',1024));n=min(requested,len(unique))
  chosen=set(random.Random(self.seed).sample(range(len(unique)),n));self.selected=[{'host_pfn':p,'gpa':g} for i,(p,g) in enumerate(unique.items()) if i in chosen]
  flagsfd=os.open('/proc/kpageflags',os.O_RDONLY);countfd=os.open('/proc/kpagecount',os.O_RDONLY)
  try:
   for s in self.selected:
    s['host_kpageflags']=struct.unpack('<Q',os.pread(flagsfd,8,s['host_pfn']*8))[0]
    s['host_kpagecount']=struct.unpack('<Q',os.pread(countfd,8,s['host_pfn']*8))[0]
  finally:os.close(flagsfd);os.close(countfd)
  rss={}
  for line in Path(f'/proc/{self.d.pid}/smaps_rollup').read_text().splitlines():
   if ':' in line and line.split(':')[0] in ('Rss','Pss','Anonymous','Private_Dirty','Private_Clean','Shared_Clean','Shared_Dirty','Swap'):
    key,rest=line.split(':',1);rss[key+'_kib']=int(rest.split()[0])
  self.counts={'resident_gpa_pages':resident_gpas,'unique_nonzero_host_pfns':len(unique),'zero_pfn_pages':masked,'pre_capture_vmm_smaps_rollup':rss,'census_ns':time.monotonic_ns()-start}
  self.design={'unit':'one representative GPA per distinct nonzero resident host PFN','population_pages':len(unique),'sample_pages':n,'requested_pages':requested,'seed':self.seed,'rule':'uniform without replacement'}
  return {s['gpa'] for s in self.selected}
 def collect(self,rows,out):
  by={r['gpa']:r for r in rows};samples=[];start=time.monotonic_ns();read_ns=0
  target=Path(out)/'compressed-sample-data.bin'
  with target.open('wb') as f:
   for s in self.selected:
    g=s['gpa'];r=by[g];t=time.monotonic_ns();raw=self.d.mem.physical(g,4096);read_ns+=time.monotonic_ns()-t
    t=time.monotonic_ns();packed=zlib.compress(raw,1);ct=time.monotonic_ns()-t
    t=time.monotonic_ns();restored=zlib.decompress(packed);dt=time.monotonic_ns()-t
    if restored!=raw:raise ValueError('codec roundtrip failed')
    offset=f.tell();f.write(packed)
    samples.append({**s,'category':r['category'],'process_names':r.get('process_names',[]),'raw_bytes':4096,'compressed_bytes':len(packed),'stored_bytes':min(4096,len(packed)+16),'saved_bytes':max(0,4096-len(packed)-16),'compress_ns':ct,'decompress_ns':dt,'roundtrip_ok':True,'in_step_footprint':r['access_status']!='outside_measured_footprint','payload_offset':offset,'payload_length':len(packed),'raw_sha256':hashlib.sha256(raw).hexdigest()})
  result={'schema':'aries.guest-compression-sample.v2','page_bytes':4096,'codec':'zlib','level':1,'metadata_bytes_per_compressed_page_assumption':16,'resident_population':self.counts,'sample_design':self.design,'samples':samples,'sampled_payload_read_ns':read_ns,'sample_and_codec_ns':time.monotonic_ns()-start,'limitations':['Conditional preserved-content capacity estimate, not implemented eviction or measured RSS savings','Host page sharing/pinning, compressor allocation, indexes beyond assumed16bytes, fault handling and copies add overhead','Census precedes attribution/payload reads; sampling can perturb host residency/cache state','Samples are uniform over distinct resident nonzero host PFNs, not over guest categories']}
  (Path(out)/'compression.json').write_text(json.dumps(result,indent=2)+'\n')
  return result
