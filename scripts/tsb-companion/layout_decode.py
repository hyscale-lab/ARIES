import argparse,json,struct,subprocess,hashlib
from pathlib import Path
parser=argparse.ArgumentParser(description='Decode compiled offsets for the pinned Linux 5.10.239 kernel.')
parser.add_argument('--offsets',type=Path,required=True,help='Raw little-endian unsigned-long attribution_layout values')
parser.add_argument('--names',type=Path,default=Path(__file__).resolve().parent/'layouts/linux-5.10.239/offset-names.json')
parser.add_argument('--kernel-image',type=Path,required=True,help='Matching unstripped guest kernel ELF')
parser.add_argument('--output',type=Path,required=True,help='Destination base-layout.json')
args=parser.parse_args()
names=json.loads(args.names.read_text());data=args.offsets.read_bytes()
values=struct.unpack('<'+'Q'*len(names),data)
layout={}
for n,v in zip(names,values):
 t,f=n.split('.',1);layout.setdefault(t,{})[f]=v
syms={}
for line in subprocess.check_output(['nm','-S',str(args.kernel_image)],text=True).splitlines():
 p=line.split()
 if p[-1] in ['vmemmap_base','page_offset_base','phys_base','init_task','init_mm','init_top_pgt','swapper_pg_dir','_stext','linux_banner','vmcoreinfo_data','mem_section']:
  syms[p[-1]]=int(p[0],16)
checks={'task_struct_size_matches_elf_init_task':layout['task_struct']['sizeof']==7040,'mm_struct_plus_initialized_cpumask_matches_elf_init_mm':layout['mm_struct']['sizeof']+8==1024}
obj={'schema':'aries.guest-layout.v1','kernel_sha256':hashlib.sha256(args.kernel_image.read_bytes()).hexdigest(),'kernel_release':'5.10.239','layout':layout,'symbols':syms,'checks':checks,'method':'Compiler-generated offsetof/sizeof for amazonlinux/linux microvm-kernel-5.10.239-261.959.amzn2 commit1696dae7327b645d2b4b860e919e431dcda0d11c with the matching embedded IKCONFIG. The kernel hash records the supplied ELF. VMCOREINFO validation and decoder ground-truth checks must be run separately.'}
args.output.write_text(json.dumps(obj,indent=2)+'\n')
print(json.dumps(obj,indent=2))
