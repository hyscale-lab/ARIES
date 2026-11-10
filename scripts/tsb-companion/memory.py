"""Kernel ELF validation; guest bytes supplied exclusively by a capture provider."""
import struct,subprocess,hashlib
from pathlib import Path
from capture_memory import CaptureMemory as GuestMemory

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
