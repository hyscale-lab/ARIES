"""Join a diagnostic guest PFN census with a neighboring paused VMM inventory."""
from bisect import bisect_right
from collections import Counter


def cross_tab(e, absent):
    g=e["guest_telemetry"]["guest"]
    p=g["page_bytes"]
    scan=g.get("pfn_scan",{})
    runs=scan.get("ranges",[])
    # Intersect the sparse resident intervals with guest flag runs. Endpoints
    # can differ across the resume/guest-scan gap; the gap is recorded below.
    present=[]
    for slot in e["ram_slots"]:
        start=slot["guest_phys_addr"]
        end=start+slot["memory_size"]
        cursor=start
        for a,b in absent:
            if b<=cursor: continue
            if a>=end: break
            if a>cursor: present.append((cursor,min(a,end)))
            cursor=max(cursor,min(b,end))
        if cursor<end: present.append((cursor,end))
    resident=sum(b-a for a,b in present)
    i=j=0
    by_flag=Counter()
    while i<len(runs) and j<len(present):
        r=runs[i]
        lo,hi=r["start_pfn"]*p,r["end_pfn_exclusive"]*p
        a,b=present[j]
        overlap=min(hi,b)-max(lo,a)
        if overlap>0: by_flag[r["class"]]+=overlap
        if hi<=b: i+=1
        if b<=hi: j+=1
    by_flag["outside_guest_scan"]+=resident-sum(by_flag.values())
    if by_flag["outside_guest_scan"]<0:
        raise ValueError("overlapping guest flag census")
    sources=g["sources"]
    buddy=None
    if "/proc/buddyinfo" in sources:
        buddy=sum(p*sum(int(n)<<order for order,n in enumerate(line.split()[4:]))
                  for line in sources["/proc/buddyinfo"].splitlines())
    meminfo={}
    for line in sources.get("/proc/meminfo","").splitlines():
        name,*v=line.split()
        if len(v)==2 and v[1]=="kB":
            meminfo[name.rstrip(":")]=int(v[0])*1024
    bounds=None if buddy is None else [max(0,buddy-(e["guest_ram_bytes"]-resident)),min(buddy,resident)]
    host=e["guest_telemetry"]
    out=dict(resident_guest_bytes=resident,guest_flag_resident_bytes=dict(by_flag),
             guest_buddy_bytes=buddy,resident_buddy_bounds_bytes=bounds,meminfo_bytes=meminfo,
             host_guest_probe_begin_ns=host["host_begin_ns"],host_guest_probe_end_ns=host["host_end_ns"],
             pfn_scan_state=scan.get("state"),probe_source_errors=g["source_errors"],
             collection_skew_ns=host["host_end_ns"]-e["freeze"]["resume_request_ns"])
    ends=[r["end_pfn_exclusive"] for r in runs]
    regular = g.get("regular_files")
    verified = set()
    if regular:
        # Each extent was present and file-backed in an inode-checked ordinary
        # guest mapping. Deduplicate alias mappings of the same file offset.
        identities = {}
        conflicts = set()
        for extent in regular["extents"] or []:
            for index in range(extent["count"]):
                gfn = extent["pfn"]+index
                identity = (extent["device"],extent["inode"],extent["file_page"]+index)
                if identities.setdefault(gfn,identity)!=identity:
                    conflicts.add(gfn)
        verified = identities.keys()-conflicts
        present_ends = [b for _,b in present]
        resident_by_class = Counter()
        for gfn in verified:
            address = gfn*p
            i = bisect_right(present_ends,address)
            if i>=len(present) or address<present[i][0]:
                continue
            j = bisect_right(ends,gfn)
            label = runs[j]["class"] if j<len(runs) and runs[j]["start_pfn"]<=gfn else "outside_guest_scan"
            resident_by_class[label]+=p
        out["mapped_regular"] = dict(guest_mapped_pages=len(verified),conflicting_pages=len(conflicts),
                    resident_bytes_by_flag_class=dict(resident_by_class),skipped=regular["skipped"],
                    processes=regular["processes"],scan_duration_ns=regular["end_ns"]-regular["begin_ns"])
    tagged=[]
    for s in e.get("content_sample",[]):
        ix=bisect_right(ends,s["gfn"])
        label=runs[ix]["class"] if ix<len(runs) and runs[ix]["start_pfn"]<=s["gfn"] else "outside_guest_scan"
        tagged.append(dict(s,guest_flag_class=label,regular_mapped=s["gfn"] in verified))
    return out,tagged
