"""Bounded external decoder memory view. No host process-memory interfaces.

The provider owns all live reads, residency sampling, first-copy identity and
capture lifetime. This class only handles guest address translation and caching.
"""
import struct

PAGE = 4096
MAX_BATCH = 256

class CaptureFailure(RuntimeError):
    """A capture cannot establish complete evidence; never classify this as a page."""

class GuestAddressError(ValueError):
    """Malformed/nonpresent guest address may be reported as unresolved metadata."""

class NeedPages(BaseException):
    """Offline closure-discovery miss; deliberately not swallowed as ValueError."""
    def __init__(self, gpas):
        self.gpas = tuple(sorted(set(gpas)))
        super().__init__(self.gpas)

class CaptureMemory:
    def __init__(self, provider, *, prefetch_pages=4):
        self.provider = provider
        self.regions = tuple(provider.regions)
        self.cache = {}
        self.prefetch_pages = max(1, min(int(prefetch_pages), MAX_BATCH))
        self.read_requests = 0
        self.requested_pages = 0
        self.consumed_pages = set()
        for region in self.regions:
            if region["guest_base"] % PAGE or region["bytes"] <= 0 or region["bytes"] % PAGE:
                raise CaptureFailure("unaligned memory region")
        bounds=sorted((r["guest_base"],r["guest_base"]+r["bytes"]) for r in self.regions)
        if any(a[1]>b[0] for a,b in zip(bounds,bounds[1:])):
            raise CaptureFailure("overlapping guest regions")

    def _region(self, gpa):
        for region in self.regions:
            if region["guest_base"] <= gpa < region["guest_base"] + region["bytes"]:
                return region
        raise GuestAddressError("GPA outside authoritative RAM regions")

    def prefetch(self, gpas):
        wanted=sorted(set(gpas)-self.cache.keys())
        for g in wanted:
            if not isinstance(g,int) or g<0 or g%PAGE:
                raise CaptureFailure("invalid GPA page address")
            self._region(g)
        for at in range(0,len(wanted),MAX_BATCH):
            batch=wanted[at:at+MAX_BATCH]
            rows=self.provider.read_pages(batch)
            self.read_requests+=1
            self.requested_pages+=len(batch)
            if set(rows)!=set(batch):
                raise CaptureFailure("provider page identities differ from request")
            for g,data in rows.items():
                if not isinstance(data,bytes) or len(data)!=PAGE:
                    raise CaptureFailure("short/invalid captured page")
                self.cache[g]=data

    def physical(self,gpa,n):
        if not isinstance(gpa,int) or not isinstance(n,int) or gpa<0 or n<0:
            raise CaptureFailure("invalid guest byte range")
        if not n:return b""
        pages=list(range(gpa&~(PAGE-1),(gpa+n+PAGE-1)&~(PAGE-1),PAGE))
        # Validate the entire range before copying or issuing requests.
        for g in pages:self._region(g)
        missing=[g for g in pages if g not in self.cache]
        for g in missing:
            if g in self.cache:continue
            region=self._region(g);end=region["guest_base"]+region["bytes"]
            limit=getattr(self.provider,"remaining_pages",self.prefetch_pages)
            width=min(self.prefetch_pages,max(1,limit))
            self.prefetch(range(g,min(g+width*PAGE,end),PAGE))
        self.consumed_pages.update(pages)
        data=b"".join(self.cache[g] for g in pages)
        return data[gpa%PAGE:gpa%PAGE+n]

    def u64p(self,gpa):return struct.unpack("<Q",self.physical(gpa,8))[0]

    def translate(self,va,pgd):
        if not isinstance(va,int) or not 0<=va<1<<64:
            raise CaptureFailure("invalid virtual address")
        # Current layouts are x86-64 four-level. Reject noncanonical input.
        if not (va < 1<<47 or va >= (1<<64)-(1<<47)):
            raise GuestAddressError("noncanonical four-level virtual address")
        if not isinstance(pgd,int) or not 0<=pgd<1<<52:
            raise GuestAddressError("invalid four-level CR3/PGD physical address")
        table=pgd & 0x000ffffffffff000
        for shift in (39,30,21,12):
            entry=self.u64p(table+((va>>shift)&511)*8)
            if not entry&1:raise ValueError("nonpresent guest PTE")
            if shift in (30,21) and entry&128:
                return (entry & (0x000ffffffffff000 & ~((1<<shift)-1)))+(va&((1<<shift)-1))
            table=entry & 0x000ffffffffff000
        return table+(va&4095)

    def virtual(self,va,n,pgd):
        data=bytearray()
        while n:
            count=min(n,PAGE-(va%PAGE))
            data.extend(self.physical(self.translate(va,pgd),count))
            va+=count;n-=count
        return bytes(data)

    def prefetch_virtual_ranges(self,ranges,pgd):
        # Bound the frontier list as well as each transport request.
        frontier=set()
        for lo,hi in ranges:
            if hi<lo:raise CaptureFailure("reversed virtual range")
            for va in range(lo&~4095,(hi+4095)&~4095,PAGE):
                g=self.translate(va,pgd)&~4095
                if g not in self.cache:frontier.add(g)
                if len(frontier)>=MAX_BATCH:
                    self.prefetch(frontier);frontier.clear()
        if frontier:self.prefetch(frontier)

    def find(self,needle,max_gpa=1<<30):
        return self.provider.find(needle,max_gpa=max_gpa)

    def resident_sample(self,seed,requested):
        return self.provider.resident_sample(seed,requested)

    def close(self):
        # Provider session lifecycle (seal/abort) is explicit and externally owned.
        pass
