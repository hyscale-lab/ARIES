"""Process/address-space users and kernel-region attribution at a paused boundary.

No ownership claim identifies the instruction or thread that accessed a page.
All memory reads use the decoder cache, so extra metadata is retained for replay.
"""
import collections,json,struct
from pathlib import Path


class OwnerAttribution:
 def __init__(self,decoder,config):
  self.d=decoder
  value=config.get('owner_layout_data',config.get('owner_layout'))
  self.meta=value if value is not None else json.loads(Path(config['owner_layout_path']).read_text())
  if self.meta['kernel_sha256']!=decoder.a['kernel_sha256']:
   raise ValueError('owner layout/kernel hash mismatch')
  self.o=self.meta['offsets'];self.errors=[]
  self.flags=decoder.l['pageflags']
  self.const=decoder.l.get('constants',{'PAGE_TYPE_BASE':0xf0000000,'PG_table':1024})
 def u64v(self,addr):return int.from_bytes(self.d.v(addr,8),'little')
 def extra(self,addr,name,size=8):return int.from_bytes(self.d.v(addr+self.o[name],size),'little')
 def _user(self,addr):
  d=self.d;pid=d.field(addr,'task_struct','pid',4);tgid=d.field(addr,'task_struct','tgid',4)
  comm=d.v(addr+d.l['task_struct']['comm'],16).split(b'\0')[0].decode(errors='replace')
  if not comm or pid>1<<24 or tgid>1<<24:raise ValueError('invalid thread identity')
  return {'pid':pid,'tgid':tgid,'comm':comm,'task_address':addr,'mm_address':d.field(addr,'task_struct','mm')}
 def users(self,tasks):
  allusers={};offset=self.o['task_struct.thread_group']
  for task in tasks:
   at=task['task_address'];root=at+offset;seen={root};group=[dict(task)]
   allusers[at]={k:task[k] for k in ('pid','tgid','comm','task_address','mm_address')}
   try:
    link=self.u64v(root)
    while link!=root:
     if link in seen or len(seen)>32768:raise ValueError('thread-group cycle/limit')
     seen.add(link);user=self._user(link-offset)
     if user['tgid']!=task['tgid']:raise ValueError('thread tgid mismatch')
     allusers[user['task_address']]=user;group.append(user);link=self.u64v(link)
   except (ValueError,OSError) as e:self.errors.append({'phase':'thread_users','pid':task['pid'],'error':str(e)})
   task['thread_users']=[{k:u[k] for k in ('pid','tgid','comm','mm_address')} for u in sorted(group,key=lambda u:u['pid'])]
  bymm=collections.defaultdict(list)
  for u in allusers.values():
   if u['mm_address']:bymm[u['mm_address']].append(u)
  return list(allusers.values()),bymm
 def tables(self,pgd,wanted):
  result=set();visited=set()
  def walk(table,level):
   if table in wanted:result.add(table)
   if (table,level) in visited:return
   visited.add((table,level))
   if len(visited)>1048576:raise ValueError('page-table traversal limit')
   if level==1:return
   # User address-space tables only. Kernel-half pages are globally shared.
   data=self.d.p(table,4096);limit=256 if level==4 else 512
   for i in range(limit):
    entry=struct.unpack_from('<Q',data,i*8)[0]
    if not entry&1 or (level in (3,2) and entry&128):continue
    child=entry&0x000ffffffffff000
    walk(child,level-1)
  walk(pgd,4);return result
 def special_regions(self):
  d=self.d;s=self.meta['symbols'];slide=d.a['kaslr_slide'];regions=[]
  for key,label in [('empty_zero_page','shared_zero_page'),('__vvar_page','shared_vvar_page')]:
   if key not in s:continue
   try:regions.append((d.translate(s[key]+slide)&~4095,4096,label))
   except (ValueError,OSError) as e:self.errors.append({'phase':'special_region','symbol':key,'error':str(e)})
  for key in ('vdso_image_64','vdso_image_32'):
   if key not in s:continue
   try:
    base=self.extra(s[key]+slide,'vdso_image.data');size=self.extra(s[key]+slide,'vdso_image.size')
    if not size:continue
    if size%4096 or size>1<<20:raise ValueError('invalid vDSO image size')
    for off in range(0,size,4096):regions.append((d.translate(base+off)&~4095,4096,'shared_'+key))
   except (ValueError,OSError) as e:self.errors.append({'phase':'special_region','symbol':key,'error':str(e)})
  return regions
 def reserved_region(self,gpa,regions):
  for base,size,label in regions:
   if base<=gpa<base+size:return label
  link=self.meta['symbols']['_stext']+gpa-self.d.a['text_phys']
  sections=[x['name'] for x in self.meta['allocated_sections'] if x['address']<=link<x['address']+x['size']]
  return 'kernel_image:'+','.join(sections) if sections else 'kernel_reserved_other'
 def compound(self,row):
  d=self.d;size=d.l['page']['sizeof'];p=d.a['vmemmap_base']+row['gpa']//4096*size
  headptr=d.field(p,'page','compound_head')
  if not headptr&1:return
  head=headptr-1;delta=head-d.a['vmemmap_base']
  if delta<0 or delta%size:raise ValueError('unaligned compound head')
  hpfn=delta//size;tpfn=row['gpa']//4096
  flags=d.field(head,'page','flags')
  if not flags&(1<<self.flags['PG_head']):raise ValueError('compound head flag missing')
  order=self.extra(head+size,'page.compound_order',1)
  if not 1<=order<=18 or not hpfn<tpfn<hpfn+(1<<order):raise ValueError('compound tail outside head order')
  mapping=d.field(head,'page','mapping');index=d.field(head,'page','index')+tpfn-hpfn
  row['compound']={'head_gpa':hpfn*4096,'order':order,'tail_index':tpfn-hpfn,'head_flags':flags,'head_mapping':mapping,'effective_index':index}
  category=None
  if flags&(1<<self.flags['PG_slab']):category='kernel_slab'
  elif mapping&1:category='anonymous'
  elif mapping and not mapping&3:
   inode=d.field(mapping,'address_space','host')
   if not inode or d.field(inode,'inode','i_mapping')!=mapping:raise ValueError('compound mapping inode backlink mismatch')
   row['file_inode_address']=inode;row['file_inode_number']=d.field(inode,'inode','i_ino');row['file_offset']=index*4096
   category='shmem' if flags&(1<<self.flags['PG_swapbacked']) else 'file_cache'
  elif flags&(1<<self.flags['PG_reserved']):category='reserved'
  if category:
   row['category_before_compound_resolution']=row['category'];row['category']=category
   row['guest_page_dirty_flag']=bool(flags&(1<<self.flags['PG_dirty']))
   row['guest_page_writeback_flag']=bool(flags&(1<<self.flags['PG_writeback']))
   row['dirty_flag_scope']='compound_head'
 def enrich(self,rows,tasks,vmas):
  self.errors=[];d=self.d;wanted={r['gpa'] for r in rows}
  users,bymm=self.users(tasks)
  leaders={t['tgid']:{'pid':t['pid'],'tgid':t['tgid'],'comm':t['comm']} for t in tasks}
  roster={}
  for mm,group in sorted(bymm.items()):
   processes={u['tgid']:leaders.get(u['tgid'],{'pid':u['tgid'],'tgid':u['tgid'],'comm':u['comm']}) for u in group}
   roster['mm:'+hex(mm)]={'mm_address':mm,
    'process_owners':[processes[k] for k in sorted(processes)],
    'thread_users':[{k:u[k] for k in ('pid','tgid','comm')} for u in sorted(group,key=lambda u:u['pid'])]}
  for v in vmas:
   v.pop('address_space_users',None)
   v['owner_ref']='mm:'+hex(v['mm_address'])
  # Store address-space identities instead of repeating every thread for each table.
  table_mms=collections.defaultdict(set);stack_owners=collections.defaultdict(list)
  for mm in sorted(bymm):
   try:
    pgd=d.translate(d.field(mm,'mm_struct','pgd'))&~4095
    for g in self.tables(pgd,wanted):table_mms[g].add(mm)
   except (ValueError,OSError) as e:self.errors.append({'phase':'page_table_owners','mm_address':mm,'error':str(e)})
  for u in users:
   try:
    stack=self.extra(u['task_address'],'task_struct.stack')
    if stack:
     for off in range(0,self.o['THREAD_SIZE'],4096):
      g=d.translate(stack+off)&~4095
      if g in wanted:stack_owners[g].append(u)
   except (ValueError,OSError) as e:self.errors.append({'phase':'kernel_stack_owner','pid':u['pid'],'error':str(e)})
  regions=self.special_regions()
  combined={}
  def process_group(mms):
   key=tuple(sorted(mms))
   if key not in combined:
    processes={}
    refs=['mm:'+hex(mm) for mm in key]
    for ref in refs:
     for owner in roster[ref]['process_owners']:processes[owner['tgid']]=owner
    group=[processes[k] for k in sorted(processes)]
    combined[key]=(refs,group,sorted({p['comm'] for p in group}))
   return combined[key]
  for row in rows:
   if row['category']=='compound_tail_unresolved':
    try:self.compound(row)
    except (ValueError,OSError) as e:row['compound_resolution_error']=str(e)
   mms={vmas[ref['vma_index']]['mm_address'] for ref in row.get('owners',[])
        if vmas[ref['vma_index']]['mm_address'] in bymm}
   kind='user_address_space';assoc=bool(mms)
   processes=[];names=[];refs=[]
   if mms:
    refs,processes,names=process_group(mms)
   elif row['gpa'] in stack_owners:
    group=stack_owners[row['gpa']];kind='kernel_stack';assoc=True
    process={u['tgid']:leaders.get(u['tgid'],{'pid':u['tgid'],'tgid':u['tgid'],'comm':u['comm']}) for u in group}
    processes=[process[k] for k in sorted(process)]
    names=sorted({p['comm'] for p in processes})
    row['kernel_stack_thread_ids']=sorted({u['pid'] for u in group})
    if row['category'] in ('unresolved','mapped_unresolved'):row['category']='kernel_stack'
   elif row['gpa'] in table_mms:
    refs,processes,names=process_group(table_mms[row['gpa']]);kind='user_page_table';assoc=True
    if row['category'] in ('unresolved','mapped_unresolved'):
     row['category_before_table_resolution']=row['category'];row['category']='kernel_page_table'
   row.pop('thread_users',None);row.pop('ownership_meaning',None)
   row['owner_refs']=refs
   row['process_owners']=processes
   row['process_names']=names
   if assoc:row['ownership_status']='associated_'+kind
   elif row['category']=='reserved':row['ownership_status']='kernel_global_no_user_mapping'
   elif row['category'].startswith('kernel_'):row['ownership_status']='kernel_owner_not_resolved'
   elif row['category']=='free_buddy':row['ownership_status']='free_no_current_owner'
   elif row['category'] in ('file_cache','shmem'):row['ownership_status']='no_present_user_mapping'
   else:row['ownership_status']='unresolved_no_present_user_mapping'
   if row['category']=='reserved':row['reserved_region']=self.reserved_region(row['gpa'],regions)
  return {'schema':'aries.owner-attribution.v3','thread_count':len(users),
   'process_leader_count':len(tasks),'unique_user_address_spaces':len(bymm),
   'address_space_roster':roster,
   'thread_roster':{str(u['pid']):{k:u[k] for k in ('pid','tgid','comm','mm_address')} for u in sorted(users,key=lambda u:u['pid'])},
   'categories':dict(collections.Counter(r['category'] for r in rows)),
   'ownership_status':dict(collections.Counter(r['ownership_status'] for r in rows)),
   'reserved_regions':dict(collections.Counter(r.get('reserved_region') for r in rows if r['category']=='reserved')),
   'compound_tails_resolved':sum('category_before_compound_resolution' in r for r in rows),
   'errors':self.errors,
   'ownership_meaning':'Boundary association; owner_refs resolve address-space users in this same capture; sharing does not identify allocating/accessing thread',
   'limits':['Owners are boundary associations, not causal access/allocation attribution','Kernel-half page tables treated as globally shared, not assigned to a user PID','Nonpresent/PROT_NONE user mappings are outside current mapping traversal','No process name is fabricated for kernel-global or unresolved pages']}
