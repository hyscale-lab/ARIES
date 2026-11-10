"""Decode retained little-endian, region-relative GPA masks for offline legacy analyses."""
import hashlib
from pathlib import Path


def expand_reading(reading, *, artifact_directory=None):
    """Relocated archives supply the phase.json sibling directory; preserve provenance."""
    if "accessed_gpas" in reading:
        return reading
    result = dict(reading)
    base = Path(artifact_directory if artifact_directory is not None else reading["artifact_directory"])
    masks = {}
    for region in reading["regions"]:
        count = region["bytes"] // 4096
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
            pages = masks.setdefault(name, [])
            for index, byte in enumerate(raw):
                for bit in range(8):
                    if byte & (1 << bit):
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
    return result
