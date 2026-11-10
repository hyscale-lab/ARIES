from pathlib import Path
import argparse,json
parser=argparse.ArgumentParser(description='Generate pinned Linux 5.10.239 offsetof/sizeof source and its ordered field names.')
parser.add_argument('--output-dir',type=Path,required=True,help='Directory for layout_offsets.c and offset-names.json')
parser.add_argument('--kernel-source',type=Path,help='Also write kernel/layout_offsets.c inside this prepared kernel source tree')
args=parser.parse_args()
base=args.output_dir
base.mkdir(parents=True,exist_ok=True)
fields={
'task_struct':['tasks','pid','tgid','comm','mm','active_mm'],
'mm_struct':['pgd','mmap','start_brk','brk','start_stack'],
'vm_area_struct':['vm_start','vm_end','vm_next','vm_flags','vm_file','vm_pgoff'],
'file':['f_path'],'path':['dentry','mnt'],'dentry':['d_name','d_parent','d_inode'],
'qstr':['name','len'],'inode':['i_ino','i_mapping','i_mode'],'address_space':['host'],
'page':['flags','_refcount','_mapcount','mapping','index','compound_head','private','page_type','lru.next','lru.prev'],
'list_head':['next','prev'],'mem_section':['section_mem_map']}
entries=[]
for s,fs in fields.items():
 entries.append((s+'.sizeof','sizeof(struct '+s+')'))
 for f in fs: entries.append((s+'.'+f,'offsetof(struct '+s+','+f+')'))
flags=['PG_locked','PG_referenced','PG_uptodate','PG_dirty','PG_lru','PG_active','PG_workingset','PG_slab','PG_owner_priv_1','PG_arch_1','PG_reserved','PG_private','PG_private_2','PG_writeback','PG_head','PG_mappedtodisk','PG_reclaim','PG_swapbacked','PG_unevictable','PG_mlocked']
for f in flags:entries.append(('pageflags.'+f,f))
for f in ['PAGE_MAPPING_ANON','PAGE_MAPPING_MOVABLE','PAGE_MAPPING_FLAGS','PAGE_TYPE_BASE','PG_buddy','PG_table','PG_offline']: entries.append(('constants.'+f,f))
c='#include <linux/sched.h>\n#include <linux/mm.h>\n#include <linux/fs.h>\n#include <linux/dcache.h>\n#include <linux/mount.h>\n#include <linux/page-flags.h>\nconst unsigned long attribution_layout[] = {\n'+',\n'.join(e[1] for e in entries)+'\n};\n'
if args.kernel_source:
 (args.kernel_source/'kernel/layout_offsets.c').write_text(c)
(base/'layout_offsets.c').write_text(c)
(base/'offset-names.json').write_text(json.dumps([e[0] for e in entries],indent=2))
