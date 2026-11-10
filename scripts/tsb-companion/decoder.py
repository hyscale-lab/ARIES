"""Boundary-state attribution; host-only metadata reads, no guest agent."""
import collections,gzip,json,struct,time
from pathlib import Path
from memory import GuestMemory,KernelImage

class GuestDecoder:
 def __init__(self,pid,vm_id,slots,config,memory_provider):
  self.pid=pid;self.vm_id=vm_id;self.slots=slots;self.config=dict(config)
  if config.get('enable_compression'):raise ValueError('payload compression is not supported by capture companion')
  for kind in ('names','owner'):
   if self.config.get(kind+'_layout_path') and not self.config.get(kind+'_layout_data'):
    self.config[kind+'_layout_data']=json.loads(Path(self.config[kind+'_layout_path']).read_text())
  self.mem=memory_provider;self.image=KernelImage(config['kernel_image'])
  self.meta=json.loads(Path(config['layout_path']).read_text());self.l=self.meta['layout']
  self.a=None;self.cache={};self.errors=[]
 def close(self):self.mem.close()
 def bootstrap(self):
  self.a=self.image.resolve(self.mem)
  if self.a['kernel_sha256']!=self.meta['kernel_sha256']:raise ValueError('layout kernel hash mismatch')
  self.info={}
  for line in self.a['vmcoreinfo'].splitlines():
   if '=' in line:
    k,v=line.split('=',1);self.info[k]=v
  for name in ('page','list_head','mem_section'):
   if 'SIZE('+name+')' in self.info:assert self.l[name]['sizeof']==int(self.info['SIZE('+name+')'])
  for field in ('flags','mapping','_refcount','_mapcount','private','compound_head'):
   assert self.l['page'][field]==int(self.info['OFFSET(page.'+field+')'])
  assert int(self.info['KERNELOFFSET'],16)==self.a['kaslr_slide']
  self.cache={};tasks,vmas,owners=self.processes(set())
  assert tasks and any(t['pid']==1 for t in tasks)
  if self.errors or not vmas:raise ValueError('bootstrap process/VMA decoding failed: '+str(self.errors))
  return {'anchors':self.a,'layout':self.meta,'attribution_config':self.config,'validated_process_count':len(tasks),'validated_vma_count':len(vmas),'tasks':tasks,'semantics':'ownership and page state at paused boundary, not access-time ownership'}
 def p(self,g,n):
  if getattr(self,'capture_abort',None) is not None and self.capture_abort.is_set():raise RuntimeError('metadata discovery canceled before guest read')
  result=bytearray()
  while n:
   base=g&~4095;k=min(n,4096-(g&4095))
   if base not in self.cache:
    if hasattr(self,'metadata_read_gpas'):self.metadata_read_gpas.add(base)
    self.cache[base]=self.mem.physical(base,4096)
   result.extend(self.cache[base][g&4095:(g&4095)+k]);g+=k;n-=k
  return bytes(result)
 def u64p(self,g):return struct.unpack('<Q',self.p(g,8))[0]
 def translate(self,va,pgd=None):
  if not isinstance(va,int) or not (0<=va<1<<47 or (1<<64)-(1<<47)<=va<1<<64):raise ValueError('noncanonical four-level virtual address')
  root=pgd if pgd is not None else self.a['kernel_pgd_phys']
  if not isinstance(root,int) or not 0<=root<1<<52:raise ValueError('invalid four-level CR3/PGD physical address')
  table=root&0x000ffffffffff000
  for shift in (39,30,21,12):
   entry=self.u64p(table+((va>>shift)&511)*8)
   if not entry&1:raise ValueError('nonpresent virtual address')
   if shift in (30,21) and entry&128:return (entry&(0x000ffffffffff000&~((1<<shift)-1)))+(va&((1<<shift)-1))
   table=entry&0x000ffffffffff000
  return table+(va&4095)
 def v(self,va,n):
  b=bytearray()
  while n:
   k=min(n,4096-(va&4095));b.extend(self.p(self.translate(va),k));va+=k;n-=k
  return bytes(b)
 def field(self,addr,typ,field,size=8,signed=False):
  return int.from_bytes(self.v(addr+self.l[typ][field],size),'little',signed=signed)
 def sym(self,name):return self.image.symbols[name]+self.a['kaslr_slide']
 def filename(self,file):
  path=file+self.l['file']['f_path'];d=self.field(path,'path','dentry');parts=[];seen=set();inode=0
  for _ in range(64):
   if d in seen:raise ValueError('dentry cycle')
   seen.add(d)
   if not inode:inode=self.field(d,'dentry','d_inode')
   q=d+self.l['dentry']['d_name'];n=self.field(q,'qstr','len',4);ptr=self.field(q,'qstr','name')
   if n>255:raise ValueError('invalid dentry length')
   name=self.v(ptr,n).decode(errors='replace')
   parent=self.field(d,'dentry','d_parent')
   if parent==d:break
   parts.append(name);d=parent
  return {'dentry_path':'/'+ '/'.join(reversed(parts)),'path_scope':'within mount; mount crossings not reconstructed','inode_address':inode,'inode_number':self.field(inode,'inode','i_ino') if inode else None}
 def walk(self,pgd,start,end):
  def descend(table,shift,base):
   first=max(0,(start-base)>>shift);last=min(511,(end-1-base)>>shift)
   if first>last:return
   data=self.p(table,4096)
   for i in range(first,last+1):
    entry=struct.unpack_from('<Q',data,i*8)[0]
    if not entry&1:continue
    va=base+(i<<shift)
    if shift==12 or (shift in (30,21) and entry&128):
     mask=(1<<shift)-1;phys=entry&(0x000ffffffffff000&~mask)
     lo=max(start,va);hi=min(end,va+(1<<shift))
     yield lo,hi,phys+(lo-va),entry
    else:yield from descend(entry&0x000ffffffffff000,shift-9,va)
  yield from descend(pgd,39,0)
 def processes(self,wanted):
  tasks=[];vmas=[];owners=collections.defaultdict(list);seen=set();mmseen=set()
  root=self.sym('init_task');at=root
  while at not in seen:
   if len(seen)>32768:raise ValueError('task list too long')
   seen.add(at);pid=self.field(at,'task_struct','pid',4);tgid=self.field(at,'task_struct','tgid',4)
   comm=self.v(at+self.l['task_struct']['comm'],16).split(b'\0')[0].decode(errors='replace')
   if pid>1<<24 or not comm:raise ValueError('invalid task layout')
   mm=self.field(at,'task_struct','mm')
   tasks.append({'pid':pid,'tgid':tgid,'comm':comm,'task_address':at,'mm_address':mm})
   if mm and mm not in mmseen:
    mmseen.add(mm)
    try:
     pgd=self.translate(self.field(mm,'mm_struct','pgd'));vma=self.field(mm,'mm_struct','mmap');vs=set();lastend=0
     while vma:
      if vma in vs or len(vs)>65536:raise ValueError('VMA cycle/limit')
      vs.add(vma);start=self.field(vma,'vm_area_struct','vm_start');end=self.field(vma,'vm_area_struct','vm_end')
      if not (lastend<=start<end<=1<<47 and start%4096==end%4096==0):raise ValueError('invalid VMA bounds')
      lastend=end;file=self.field(vma,'vm_area_struct','vm_file');flags=self.field(vma,'vm_area_struct','vm_flags')
      row={'pid':pid,'tgid':tgid,'comm':comm,'mm_address':mm,'start':start,'end':end,'flags':flags,'pgoff':self.field(vma,'vm_area_struct','vm_pgoff'),'file_address':file,'pgd_phys':pgd}
      if file:
       try:row['file']=self.filename(file)
       except (ValueError,OSError) as e:row['file_error']=str(e)
      index=len(vmas);vmas.append(row)
      if wanted:
       for lo,hi,g,entry in self.walk(pgd,start,end):
        for phys in range(g,g+hi-lo,4096):
         if phys in wanted:owners[phys].append({'vma_index':index,'virtual_address':lo+phys-g,'pte':entry})
      vma=self.field(vma,'vm_area_struct','vm_next')
    except (ValueError,OSError) as e:self.errors.append({'phase':'process_mappings','pid':pid,'error':str(e)})
   at=self.field(at,'task_struct','tasks')-self.l['task_struct']['tasks']
  if at!=root:raise ValueError('task list closes away from init_task')
  mmowners=collections.defaultdict(list)
  for t in tasks:
   if t['mm_address']:mmowners[t['mm_address']].append({k:t[k] for k in ('pid','tgid','comm')})
  for v in vmas:v['process_owners']=mmowners[v['mm_address']]
  return tasks,vmas,owners
 def free_pages(self,wanted):
  info=self.info;free=set();zones=[]
  node_array=int(info['SYMBOL(node_data)'],16);count=int(info['LENGTH(node_data)'])
  nodes=struct.unpack('<'+'Q'*count,self.v(node_array,8*count))
  for node in nodes:
   if not node:continue
   nz=int.from_bytes(self.v(node+int(info['OFFSET(pglist_data.nr_zones)']),4),'little')
   if nz>8:raise ValueError('invalid zone count')
   for zi in range(nz):
    zone=node+int(info['OFFSET(pglist_data.node_zones)'])+zi*int(info['SIZE(zone)'])
    for order in range(int(info['LENGTH(zone.free_area)'])):
     fa=zone+int(info['OFFSET(zone.free_area)'])+order*int(info['SIZE(free_area)'])
     for mt in range(int(info['LENGTH(free_area.free_list)'])):
      head=fa+int(info['OFFSET(free_area.free_list)'])+mt*16
      link=int.from_bytes(self.v(head,8),'little');seen=set()
      while link!=head:
       if link in seen or len(seen)>1048576:raise ValueError('free list cycle')
       seen.add(link);page=link-int(info['OFFSET(page.lru)']);delta=page-self.a['vmemmap_base']
       if delta<0 or delta%self.l['page']['sizeof']:raise ValueError('invalid free page pointer')
       gpa=delta//self.l['page']['sizeof']*4096
       for g in range(gpa,gpa+(4096<<order),4096):
        if g in wanted:free.add(g)
       link=int.from_bytes(self.v(link,8),'little')
  return free
 def capture_metadata(self,reading):
  if self.a is None:raise ValueError('bootstrap required')
  start=time.monotonic_ns();self.cache={};self.errors=[];self.metadata_read_gpas=set()
  manual=set(reading.get('selected_gpas',[])) if reading.get('selection_scope')=='explicit_gpas' else set()
  observed=set(reading['accessed_gpas']);unknown={int(g):why for g,why in reading['unknown_gpas'].items()}
  additional=set(reading.get('additional_gpas',[]))
  wanted=observed|set(unknown)|additional|manual
  sample=None;sample_gpas=set(self.config.get('replay_sample_gpas',[]))
  if self.config.get('enable_resident_sampling') and 'replay_sample_gpas' not in self.config:
   from compression import ResidentSample
   sample=ResidentSample(self,reading);sample_gpas=sample.census()
  allwanted=wanted|sample_gpas
  # Known struct-page frontiers are fetched in bounded batches before per-row decoding.
  prefetch=getattr(self.mem,'prefetch_virtual_ranges',None)
  if prefetch and allwanted:
   size=self.l['page']['sizeof'];ranges=[];last=None
   for g in sorted(allwanted):
    lo=self.a['vmemmap_base']+(g//4096)*size;hi=lo+size
    if ranges and lo==ranges[-1][1]:ranges[-1]=(ranges[-1][0],hi)
    else:ranges.append((lo,hi))
   prefetch(ranges,self.a['kernel_pgd_phys'])
  tasks,vmas,owners=self.processes(allwanted)
  try:free=self.free_pages(allwanted)
  except (ValueError,OSError) as e:free=set();self.errors.append({'phase':'free_lists','error':str(e)})
  rows=[];counts=collections.Counter();flagbits=self.l['pageflags']
  for g in sorted(allwanted):
   row={'gpa':g,'access_status':'manual_selection' if g in manual else 'observed' if g in observed else 'assumed' if g in unknown else 'dirty_only_evidence' if g in additional else 'outside_measured_footprint','access_unknown_reason':unknown.get(g),'owners':owners.get(g,[])}
   if reading.get('additional_access_sources') is not None:
    row['additional_access_sources']=reading['additional_access_sources'].get(str(g),[])
   try:
    page=self.a['vmemmap_base']+(g//4096)*self.l['page']['sizeof'];raw=self.v(page,self.l['page']['sizeof'])
    def value(f,n=8,signed=False):return int.from_bytes(raw[self.l['page'][f]:self.l['page'][f]+n],'little',signed=signed)
    flags=value('flags');mapping=value('mapping');head=value('compound_head')
    row.update(page_flags=flags,refcount=value('_refcount',4,True),mapcount_raw=value('_mapcount',4,True),mapping=mapping,index=value('index'),guest_page_dirty_flag=bool(flags&(1<<flagbits['PG_dirty'])),guest_page_writeback_flag=bool(flags&(1<<flagbits['PG_writeback'])))
    if g in free:category='free_buddy'
    elif head&1:category='compound_tail_unresolved'
    elif flags&(1<<flagbits['PG_slab']):category='kernel_slab'
    elif ((row['mapcount_raw'] & 0xffffffff) & (0xf0000000|1024))==0xf0000000:category='kernel_page_table'
    elif mapping&1:category='anonymous'
    elif mapping and not mapping&3:
     inode=self.field(mapping,'address_space','host')
     if not inode or self.field(inode,'inode','i_mapping')!=mapping:raise ValueError('page mapping/inode backlink mismatch')
     row['file_inode_address']=inode;row['file_inode_number']=self.field(inode,'inode','i_ino')
     row['file_offset']=value('index')*4096
     category='shmem' if flags&(1<<flagbits['PG_swapbacked']) else 'file_cache'
    elif flags&(1<<flagbits['PG_reserved']):category='reserved'
    elif owners.get(g):category='mapped_unresolved'
    else:category='unresolved'
    row['category']=category
   except (ValueError,OSError) as e:row['category']='unresolved';row['classification_error']=str(e)
   rows.append(row)
  owner_summary=None;files={};names_summary=None
  if self.config.get('owner_layout_data') or self.config.get('owner_layout_path'):
   from owners import OwnerAttribution
   owner_summary=OwnerAttribution(self,self.config).enrich(rows,tasks,vmas)
  if self.config.get('names_layout_data') or self.config.get('names_layout_path'):
   from file_names import FileNames
   helper=FileNames(self,self.config);helper.begin(tasks,vmas)
   for row in rows:
    inode=row.get('file_inode_address')
    if row['category'] in ('file_cache','shmem') and inode:
     ref=hex(inode)
     if ref not in files:files[ref]=helper.resolve(inode)
     row['file_ref']=ref;row['file_naming_status']=files[ref]['naming_status']
   names_summary=helper.metadata()
  allrows=rows
  rows=[row for row in allrows if row['gpa'] in wanted]
  counts=collections.Counter(row['category'] for row in rows)
  pages=sorted(self.cache)
  result={'schema':'aries.guest-attribution.v2' if self.config.get('names_layout_data') else 'aries.guest-attribution.v1','vm_id':self.vm_id,'group_id':reading.get('tool_call_id'),'pages':len(rows),'categories':dict(counts),'observed_pages':len(observed),'assumed_pages':len(unknown),'pages_with_process_mapping':sum(bool(r['owners']) for r in rows),'process_count':len(tasks),'vma_count':len(vmas),'metadata_pages_copied':len(pages),'resident_sample_pages':len(sample_gpas),'file_naming_counts':dict(collections.Counter(row.get('file_naming_status','missing_inode_identity') for row in rows if row['category']=='file_cache')),'owner_summary':owner_summary,'file_namespace_errors':names_summary['errors'] if names_summary else [],'errors':self.errors,'capture_and_decode_ns':time.monotonic_ns()-start,'semantics':'paused-boundary state; not access-time owner; file paths are mount-relative; refcount is not reclaimability; free excludes per-CPU lists','limitations':['VMM capture coherence is qualified by its seal/finalized receipt; without dirty validation only declared pause/drain quiescence is established','Unset page dirty flag is not proof of clean/discardable content; writable PTE dirty bits and backing-file consistency require separate checks','Owner traversal covers process leaders, all leaders sharing each decoded mm, present user PTEs only; PROT_NONE/swapped mappings excluded','Owner coverage is partial if process_mappings errors are recorded','Kernel stacks/unmapped allocations may remain unresolved','Metadata discovery and artifact publication have separate timing; boundary consistency requires caller validation','Anonymous vs file-backed classification uses guest page metadata, not VMA pathname alone']}
  if manual:result['manually_selected_pages']=len(manual)
  if additional:result['dirty_only_pages']=len(additional-observed-set(unknown))
  return {'summary':result,'rows':rows,'allrows':allrows,'sample_gpas':sample_gpas,
          'sample':sample,'owners':owner_summary,'files':files,'names':names_summary,
          'tasks':tasks,'vmas':vmas,'metadata':dict(self.cache)}
 def publish_capture(self,bundle,output_dir):
  start=time.monotonic_ns();out=Path(output_dir);out.mkdir(parents=True,exist_ok=True)
  def write(name,value):(out/name).write_text(json.dumps(value,indent=2)+'\n')
  if bundle['owners'] is not None:write('owners.json',bundle['owners'])
  if bundle['names'] is not None:
   write('files.json',bundle['files']);write('file-namespaces.json',bundle['names'])
  sample=bundle['sample']
  if sample:
   write('resident-census.json',{'resident_population':sample.counts,'sample_design':sample.design,'selection':sample.selected})
   if self.config.get('enable_compression',False):sample.collect(bundle['allrows'],out)
  if bundle['sample_gpas']:
   with gzip.open(out/'resident-sample.jsonl.gz','wt',compresslevel=1) as f:
    for row in bundle['allrows']:
     if row['gpa'] in bundle['sample_gpas']:f.write(json.dumps(row)+'\n')
  with gzip.open(out/'pages.jsonl.gz','wt',compresslevel=1) as f:
   for row in bundle['rows']:f.write(json.dumps(row)+'\n')
  write('processes.json',bundle['tasks']);write('vmas.json',bundle['vmas'])
  pages=sorted(bundle['metadata'])
  with gzip.open(out/'metadata-pages.bin.gz','wb',compresslevel=1) as f:
   for g in pages:f.write(bundle['metadata'][g])
  write('metadata-index.json',{'page_bytes':4096,'gpas':pages})
  result=dict(bundle['summary']);result['publication_ns']=time.monotonic_ns()-start
  write('summary.json',result)
  return result
 def capture(self,reading,output_dir):
  return self.publish_capture(self.capture_metadata(reading),output_dir)
