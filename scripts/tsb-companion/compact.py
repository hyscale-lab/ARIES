"""Decode validated region-relative GPA masks for attribution and offline analysis."""
import hashlib
from pathlib import Path

_BIT_OFFSETS = tuple(tuple(bit for bit in range(8) if byte & (1 << bit))
                     for byte in range(256))


def expand_reading(reading, *, artifact_directory=None):
    """Relocated archives supply the phase.json sibling directory; preserve provenance."""
    if "accessed_gpas" in reading:
        return reading
    result = dict(reading)
    base = Path(artifact_directory if artifact_directory is not None else reading["artifact_directory"])
    # Older batch wrappers replaced the mechanism schema but retained its VM generation.
    native = reading.get("schema") == "aries.vmm-tracer.compact.v1" or "vm_generation" in reading
    regions = reading["regions"]
    bounds = []
    for region in regions:
        start, size = region["guest_base"], region["bytes"]
        if (type(start) is not int or type(size) is not int or start < 0
                or size <= 0 or start % 4096 or size % 4096):
            raise ValueError("invalid compact RAM region")
        bounds.append((start, start + size))
    bounds.sort()
    if not bounds or any(a[1] > b[0] for a, b in zip(bounds, bounds[1:])):
        raise ValueError("missing or overlapping compact RAM regions")
    masks = {}
    for region in regions:
        count = region["bytes"] // 4096
        region_masks = {}
        raw_masks = {}
        for name, rec in region["masks"].items():
            path = Path(rec["file"])
            if path.is_absolute() or ".." in path.parts:
                raise ValueError("compact bitmap path escapes artifact directory")
            raw = (base / path).read_bytes()
            if len(raw) != ((count + 63) // 64) * 8 or hashlib.sha256(raw).hexdigest() != rec["sha256"]:
                raise ValueError("compact bitmap integrity failure")
            bits = int.from_bytes(raw, "little")
            if bits >> count or bits.bit_count() != rec["pages"]:
                raise ValueError("compact bitmap bounds/population failure")
            region_masks[name] = bits
            raw_masks[name] = raw
        required = {"access_observed", "access_unknown", "access_idle",
                    "kvm_dirty", "vmm_dirty", "combined_dirty"}
        if native:
            required.add("access_absent")
        if not required <= region_masks.keys():
            raise ValueError("compact reading missing required masks")
        classified = 0
        for name in ("access_observed", "access_unknown", "access_idle", "access_absent"):
            bits = region_masks.get(name, 0)
            if classified & bits:
                raise ValueError("compact access classifications overlap")
            classified |= bits
        if native and classified != (1 << count) - 1:
            raise ValueError("compact access classifications do not cover RAM")
        explained = 0
        for name, bits in region_masks.items():
            if not name.startswith("unknown_"):
                continue
            if name == "unknown_" or explained & bits:
                raise ValueError("compact unknown reasons overlap or are empty")
            explained |= bits
        if explained != region_masks["access_unknown"]:
            raise ValueError("compact unknown-reason partition mismatch")
        if region_masks["combined_dirty"] != (region_masks["kvm_dirty"] | region_masks["vmm_dirty"]):
            raise ValueError("compact dirty union mismatch")
        for name, raw in raw_masks.items():
            # Absence is validated above; consumers need no list of every absent GPA.
            if name == "access_absent":
                continue
            pages = masks.setdefault(name, [])
            for index, byte in enumerate(raw):
                for bit in _BIT_OFFSETS[byte]:
                    pages.append(region["guest_base"] + (index * 8 + bit) * 4096)
    result["accessed_gpas"] = sorted(masks["access_observed"])
    result["idle_gpas"] = sorted(masks["access_idle"])
    result["unknown_gpas"] = {str(g): name[len("unknown_"):] for name, pages in masks.items()
                              if name.startswith("unknown_") for g in pages}
    if set(map(int, result["unknown_gpas"])) != set(masks["access_unknown"]):
        raise ValueError("compact unknown-reason partition mismatch")
    source_sets = {name: set(masks[name]) for name in ("kvm_dirty", "vmm_dirty")}
    result["dirty_gpas"] = sorted(masks["combined_dirty"])
    result["additional_gpas"] = result["dirty_gpas"]
    result["additional_access_sources"] = {str(g): [label for label, name in (("kvm_dirty", "kvm_dirty"), ("vmm_potential_write", "vmm_dirty")) if g in source_sets[name]] for g in result["dirty_gpas"]}
    result["counted_policy"] = "access_observed union access_unknown union conservative combined_dirty"
    if native:
        counts = {"observed_pages": len(result["accessed_gpas"]),
                  "unknown_pages": len(result["unknown_gpas"]),
                  "dirty_pages": len(result["dirty_gpas"]),
                  "counted_pages": len(set(result["accessed_gpas"]) |
                      set(map(int, result["unknown_gpas"])) | set(result["dirty_gpas"]))}
        for name, expected in counts.items():
            if name in reading and (type(reading[name]) is not int or reading[name] != expected):
                raise ValueError("compact summary/mask count mismatch: " + name)
    return result
