import copy,struct,unittest
from owners import OwnerAttribution

class Fake:
 def __init__(self):
  self.a={'kernel_sha256':'test','vmemmap_base':0x100000,'text_phys':0x1000,'kaslr_slide':0}
  self.l={'pageflags':{'PG_head':16,'PG_slab':9,'PG_reserved':12,'PG_dirty':3,'PG_writeback':15,'PG_swapbacked':19},'constants':{'PAGE_TYPE_BASE':0xf0000000,'PG_table':1024},'page':{'sizeof':64},'task_struct':{'comm':100}}
  self.fields={};self.bytes={};self.phys={};self.translations={}
 def field(self,a,t,f,size=8,signed=False):return self.fields[(a,t,f)]
 def v(self,a,n):
  if (a,n) in self.bytes:return self.bytes[(a,n)]
  raise ValueError('missing fake virtual bytes')
 def p(self,a,n):return self.phys[a][:n]
 def translate(self,a):return self.translations.get(a,a)

def layout():
 return {'kernel_sha256':'test','offsets':{'task_struct.thread_group':8,'task_struct.stack':24,'THREAD_SIZE':4096,'vdso_image.data':0,'vdso_image.size':8,'page.compound_order':17},'symbols':{'_stext':0x1000},'allocated_sections':[]}
def u64(x):return x.to_bytes(8,'little')
def task(d,addr,pid,tgid,mm,comm,nextaddr):
 for f,val in [('pid',pid),('tgid',tgid),('mm',mm)]:d.fields[addr,'task_struct',f]=val
 d.bytes[addr+100,16]=comm.encode().ljust(16,b'\0');d.bytes[addr+8,8]=u64(nextaddr+8);d.bytes[addr+24,8]=u64(0)
 return {'task_address':addr,'pid':pid,'tgid':tgid,'mm_address':mm,'comm':comm}

class OwnerTests(unittest.TestCase):
 def test_inline_layout_and_hash_gate(self):
  d=Fake();OwnerAttribution(d,{'owner_layout_data':layout()})
  bad=layout();bad['kernel_sha256']='wrong'
  with self.assertRaisesRegex(ValueError,'hash mismatch'):OwnerAttribution(d,{'owner_layout_data':bad})
 def test_shared_mm_threads_and_reserved_not_fabricated(self):
  d=Fake();a=task(d,0x100,10,10,0x800,'leader',0x200);task(d,0x200,11,10,0x800,'worker',0x100);b=task(d,0x300,20,20,0x800,'clone-vm',0x300)
  d.fields[0x800,'mm_struct','pgd']=0x9000;d.phys[0x9000]=bytes(4096)
  h=OwnerAttribution(d,{'owner_layout_data':layout()})
  rows=[{'gpa':0x3000,'category':'anonymous','owners':[{'vma_index':0}]},{'gpa':0x4000,'category':'reserved','owners':[]},{'gpa':0x9000,'category':'unresolved','owners':[]}]
  vmas=[{'mm_address':0x800}];summary=h.enrich(rows,[a,b],vmas)
  self.assertNotIn('thread_users',rows[0])
  self.assertNotIn('ownership_meaning',rows[0])
  self.assertEqual(rows[0]['owner_refs'],['mm:0x800'])
  self.assertEqual([u['pid'] for u in summary['address_space_roster']['mm:0x800']['thread_users']],[10,11,20])
  self.assertEqual(vmas[0]['owner_ref'],'mm:0x800')
  self.assertNotIn('address_space_users',vmas[0])
  self.assertEqual(rows[0]['process_names'],['clone-vm','leader'])
  self.assertEqual(rows[1]['process_owners'],[])
  self.assertEqual(rows[1]['ownership_status'],'kernel_global_no_user_mapping')
  self.assertEqual(rows[2]['category'],'kernel_page_table');self.assertEqual(rows[2]['process_names'],['clone-vm','leader'])
  self.assertEqual(summary['errors'],[])
 def test_kernel_stack_association(self):
  d=Fake();a=task(d,0x100,7,7,0,'kworker',0x100);d.bytes[0x100+24,8]=u64(0x5000)
  row={'gpa':0x5000,'category':'unresolved','owners':[]}
  summary=OwnerAttribution(d,{'owner_layout_data':layout()}).enrich([row],[a],[])
  self.assertEqual(row['category'],'kernel_stack');self.assertEqual(row['process_names'],['kworker'])
  self.assertEqual(row['kernel_stack_thread_ids'],[7]);self.assertEqual(summary['thread_roster']['7']['comm'],'kworker')
  self.assertEqual(summary['errors'],[])
 def test_page_tables_skip_kernel_half_and_huge_leaf(self):
  d=Fake();root=bytearray(4096);struct.pack_into('<Q',root,0,0x2001);struct.pack_into('<Q',root,300*8,0xdead001)
  child=bytearray(4096);struct.pack_into('<Q',child,0,0x40000081)
  d.phys[0x1000]=root;d.phys[0x2000]=child
  h=OwnerAttribution(d,{'owner_layout_data':layout()})
  self.assertEqual(h.tables(0x1000,{0x1000,0x2000,0xdead000}),{0x1000,0x2000})
 def test_compound_tail_file_offset_and_dirty_scope(self):
  d=Fake();h=OwnerAttribution(d,{'owner_layout_data':layout()});head=0x100000+10*64;tail=head+2*64
  d.fields[tail,'page','compound_head']=head+1;d.fields[head,'page','flags']=(1<<16)|(1<<3)
  d.bytes[head+64+17,1]=bytes([2]);d.fields[head,'page','mapping']=0x8000;d.fields[head,'page','index']=100
  d.fields[0x8000,'address_space','host']=0x9000;d.fields[0x9000,'inode','i_mapping']=0x8000;d.fields[0x9000,'inode','i_ino']=42
  row={'gpa':12*4096,'category':'compound_tail_unresolved'}
  h.compound(row)
  self.assertEqual(row['category'],'file_cache');self.assertEqual(row['file_offset'],102*4096)
  self.assertEqual(row['file_inode_number'],42);self.assertTrue(row['guest_page_dirty_flag']);self.assertEqual(row['dirty_flag_scope'],'compound_head')
 def test_compound_invalid_order_is_rejected(self):
  d=Fake();h=OwnerAttribution(d,{'owner_layout_data':layout()});head=0x100000+10*64;tail=head+2*64
  d.fields[tail,'page','compound_head']=head+1;d.fields[head,'page','flags']=1<<16;d.bytes[head+64+17,1]=bytes([1])
  with self.assertRaisesRegex(ValueError,'outside head order'):h.compound({'gpa':12*4096,'category':'compound_tail_unresolved'})
 def test_reserved_sections_not_process_ownership(self):
  d=Fake();m=layout();m['allocated_sections']=[{'name':'.text','address':0x1000,'size':4096}]
  h=OwnerAttribution(d,{'owner_layout_data':m})
  self.assertEqual(h.reserved_region(0x1000,[]),'kernel_image:.text')
  self.assertEqual(h.reserved_region(0x5000,[(0x5000,4096,'shared_zero_page')]),'shared_zero_page')

if __name__=='__main__':unittest.main()
