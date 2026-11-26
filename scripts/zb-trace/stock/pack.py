"""Join a v0.2.5 local startup manifest to the checkpoint's guest PFN census."""
import argparse
from bisect import bisect_right
from collections import Counter
import hashlib
import json
from pathlib import Path
import struct

PAGE = 4096


def pages(data, budget):
    header = struct.Struct("<8sIIQII")
    magic, version, size, memory, n_prefix, n_range = header.unpack_from(data)
    if (magic, version, size) != (b"AENVMF01", 4, header.size):
        raise ValueError("unsupported startup manifest")
    if max(n_prefix, n_range) > 1 << 20 or len(data) != size + 8*n_prefix + 16*n_range:
        raise ValueError("invalid manifest size")
    prefix_end = size + 8*n_prefix
    prefix = dict.fromkeys(p for p, in struct.iter_unpack("<Q", data[size:prefix_end]))
    ranges = list(struct.iter_unpack("<QQ", data[prefix_end:]))
    end = 0
    for offset, length in sorted(ranges):
        if offset < end or offset % PAGE or not length or length % PAGE or offset + length > memory:
            raise ValueError("invalid memory ranges")
        end = offset + length
    if any(p % PAGE or p + PAGE > memory for p in prefix) or budget < 0:
        raise ValueError("invalid prefix or budget")
    remaining = budget // PAGE
    for p in prefix:
        if remaining == 0:
            return
        yield p
        remaining -= 1
    for offset, length in ranges:
        for p in range(offset, offset + length, PAGE):
            if remaining == 0:
                return
            if p not in prefix:
                yield p
                remaining -= 1


def classify(data, budget, slots, census):
    # Slots join the memory-device file offset to the actual KVM guest address.
    slots = sorted(slots, key=lambda s: s["image_offset"])
    slot_ends = []
    for s in slots:
        off, start, size = (s[k] for k in ("image_offset", "guest_phys_addr", "memory_size"))
        if min(off, start) < 0 or size <= 0 or (off | start | size) % PAGE or (slot_ends and off < slot_ends[-1]):
            raise ValueError("invalid or overlapping image slots")
        slot_ends.append(off + size)
    if census["page_bytes"] != PAGE:
        raise ValueError("guest page size mismatch")
    runs = census["pfn_scan"]["ranges"]
    ends = []
    for r in runs:
        if not 0 <= r["start_pfn"] < r["end_pfn_exclusive"] or (ends and r["start_pfn"] < ends[-1]):
            raise ValueError("invalid or overlapping census")
        ends.append(r["end_pfn_exclusive"])
    labels = {"buddy-flagged":"buddy-free", "slab-flagged":"slab", "anonymous-flagged":"anonymous",
              "page-table-flagged":"other", **{"file-cache-candidate-"+s:"page-cache-candidate"
              for s in ("clean", "dirty", "writeback")}}
    # Only an unambiguous mapped regular file can upgrade a file-cache candidate.
    regular = {}
    for extent in (census.get("regular_files") or {}).get("extents") or []:
        for i in range(extent["count"]):
            pfn = extent["pfn"] + i
            identity = (extent["device"], extent["inode"], extent["file_page"] + i)
            if regular.setdefault(pfn, identity) != identity:
                regular[pfn] = None
    totals = Counter(dict.fromkeys(("page-cache","anonymous","buddy-free","slab","other","unknown"), 0))
    candidates = 0
    for off in pages(data, budget):
        label = "unknown"
        i = bisect_right(slot_ends, off)
        if i < len(slots) and off >= slots[i]["image_offset"]:
            pfn = (slots[i]["guest_phys_addr"] + off - slots[i]["image_offset"]) // PAGE
            j = bisect_right(ends, pfn)
            if j < len(runs) and pfn >= runs[j]["start_pfn"]:
                label = labels.get(runs[j]["class"], "unknown")
                if label == "page-cache-candidate":
                    candidates += PAGE
                    label = "unknown"
                if regular.get(pfn) is not None and runs[j]["class"].startswith("file-cache-candidate-"):
                    label = "page-cache"
        totals[label] += PAGE
    payload = sum(totals.values())
    return dict(manifest_sha256=hashlib.sha256(data).hexdigest(), manifest_bytes=len(data),
                planned_payload_bytes=payload, class_bytes=totals,
                class_shares={k:v/payload if payload else None for k,v in totals.items()},
                file_cache_candidate_bytes=candidates)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("manifest", "slots", "guest_census"):
        parser.add_argument(name, type=Path)
    parser.add_argument("--budget-bytes", type=int, required=True)
    args = parser.parse_args()
    print(json.dumps(classify(args.manifest.read_bytes(), args.budget_bytes,
                              json.loads(args.slots.read_text()), json.loads(args.guest_census.read_text()))))
