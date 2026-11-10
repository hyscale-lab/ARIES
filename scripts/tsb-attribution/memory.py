"""Read guest RAM and resolve kernel anchors without guest execution."""
import os,struct,subprocess,json,time,hashlib
from pathlib import Path

class GuestMemory:
 def __init__(self,pid,slots):
  self.pid=pid;self.slots=slots
  self.fd=os.open(f'/proc/{pid}/mem',os.O_RDONLY)
  self.pm=os.open(f'/proc/{pid}/pagemap',os.O_RDONLY)
 def close(self):
  os.close(self.fd);os.close(self.pm)
 def physical(self,gpa,n):
  for s in self.slots:
   off=gpa-s['guest_phys_addr']
   if 0<=off and off+n<=s['memory_size']:
    b=os.pread(self.fd,n,s['userspace_addr']+off)
    if len(b)!=n: raise ValueError('short guest RAM read')
    return b
  raise ValueError(f'GPA outside RAM: {gpa:x}+{n}')
 def u64p(self,gpa):return struct.unpack('<Q',self.physical(gpa,8))[0]
 def translate(self,va,pgd):
  table=pgd & 0x000ffffffffff000
  for shift in (39,30,21,12):
   entry=self.u64p(table+((va>>shift)&511)*8)
   if not entry&1:raise ValueError(f'nonpresent PTE va={va:x} shift={shift}')
   if shift in (30,21) and entry&128:
    return (entry & (0x000ffffffffff000 & ~((1<<shift)-1)))+(va&((1<<shift)-1))
   table=entry&0x000ffffffffff000
  return table+(va&4095)
 def virtual(self,va,n,pgd):
  b=bytearray()
  while n:
   k=min(n,4096-(va&4095));b.extend(self.physical(self.translate(va,pgd),k));va+=k;n-=k
  return bytes(b)
 def resident_runs(self,max_gpa=1<<30):
  for s in self.slots:
   base=s['guest_phys_addr'];size=min(s['memory_size'],max(0,max_gpa-base))
   if not size:continue
   n=size//4096;start=None
   for block in range(0,n,8192):
    k=min(8192,n-block)
    data=os.pread(self.pm,k*8,(s['userspace_addr']//4096+block)*8)
    for j,(entry,) in enumerate(struct.iter_unpack('<Q',data)):
     g=base+(block+j)*4096
     if entry&(1<<63):
      if start is None:start=g
     elif start is not None:yield start,g;start=None
   if start is not None:yield start,base+n*4096
 def find(self,needle,max_gpa=1<<30):
  matches=[]
  for lo,hi in self.resident_runs(max_gpa):
   tail=b''
   for g in range(lo,hi,1<<20):
    b=self.physical(g,min(1<<20,hi-g));buf=tail+b;at=0
    while True:
     k=buf.find(needle,at)
     if k<0:break
     matches.append(g-len(tail)+k);at=k+1
    tail=buf[-(len(needle)-1):]
  return matches

class KernelImage:
 def __init__(self,path):
  self.path=str(path);self.data=Path(path).read_bytes();self.symbols={}
  for line in subprocess.check_output(['readelf','-sW',self.path],text=True).splitlines():
   fields=line.split()
   if len(fields)>=8 and fields[0].endswith(':'):
    try:self.symbols[fields[7]]=int(fields[1],16)
    except ValueError:pass
  h=struct.unpack_from('<16sHHIQQQIHHHHHH',self.data)
  self.segments=[struct.unpack_from('<IIQQQQQQ',self.data,h[5]+i*h[9]) for i in range(h[10])]
 def bytes(self,va,n):
  for typ,flags,off,vaddr,paddr,filesz,memsz,align in self.segments:
   if typ==1 and vaddr<=va and va+n<=vaddr+filesz:return self.data[off+va-vaddr:off+va-vaddr+n]
  raise ValueError('ELF address outside loadable segment')
 def resolve(self,mem):
  s=self.symbols;text=s['_stext'];banner=self.bytes(s['linux_banner'],163).split(b'\0')[0]
  candidates=mem.find(banner);valid=[]
  for banner_phys in candidates:
   try:
    text_phys=banner_phys-(s['linux_banner']-text)
    def symp(name):return text_phys+s[name]-text
    phys_base=mem.u64p(symp('phys_base'))
    slide=(text_phys-(text-0xffffffff80000000)-phys_base)&((1<<64)-1)
    if slide>=1<<31 or slide%(1<<21):continue
    pgd=symp('init_top_pgt')
    assert mem.virtual(s['linux_banner']+slide,len(banner),pgd)==banner
    assert mem.translate(text+slide,pgd)==text_phys
    page_offset=mem.u64p(symp('page_offset_base'));vmemmap=mem.u64p(symp('vmemmap_base'))
    assert mem.translate(page_offset+text_phys,pgd)==text_phys
    note_ptr=mem.u64p(symp('vmcoreinfo_data'))
    note=mem.virtual(note_ptr,4096,pgd).split(b'\0')[0].decode(errors='replace')
    valid.append({'text_phys':text_phys,'kaslr_slide':slide,'kernel_pgd_phys':pgd,'page_offset_base':page_offset,'vmemmap_base':vmemmap,'phys_base':phys_base,'banner':banner.decode(),'vmcoreinfo':note,'kernel_sha256':hashlib.sha256(self.data).hexdigest(),'banner_candidates':candidates})
   except (ValueError,AssertionError,OSError):continue
  if len(valid)!=1:raise ValueError(f'expected one validated kernel anchor, got {len(valid)} from {candidates}')
  return valid[0]
