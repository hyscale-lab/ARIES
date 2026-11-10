#!/usr/bin/env python3
"""Replay attribution from retained metadata bytes; never opens a live VMM."""
import argparse
import gzip
import hashlib
import json
import zlib
from pathlib import Path
from types import SimpleNamespace

from decoder import GuestDecoder


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


class FrozenMemory:
    def __init__(self, index_path, data_path):
        index = json.loads(Path(index_path).read_text())
        if index["page_bytes"] != 4096:
            raise ValueError("unsupported retained page size")
        gpas = index["gpas"]
        if len(set(gpas)) != len(gpas) or any(g < 0 or g % 4096 for g in gpas):
            raise ValueError("invalid retained GPA index")
        self.pages = {}
        with gzip.open(data_path, "rb") as stream:
            for gpa in gpas:
                data = stream.read(4096)
                if len(data) != 4096:
                    raise ValueError("short retained metadata")
                self.pages[gpa] = data
            if stream.read(1):
                raise ValueError("trailing retained metadata")
        self.requested = set()

    def physical(self, gpa, length):
        result = bytearray()
        while length:
            base = gpa & ~4095
            amount = min(length, 4096 - (gpa & 4095))
            if base not in self.pages:
                raise ValueError(f"metadata page not retained: {base:#x}")
            self.requested.add(base)
            result.extend(self.pages[base][gpa & 4095:(gpa & 4095) + amount])
            gpa += amount
            length -= amount
        return bytes(result)

    def close(self):
        pass


def load_rows(path):
    with gzip.open(path, "rt") as stream:
        rows = [json.loads(line) for line in stream if line.strip()]
    if len({r["gpa"] for r in rows}) != len(rows):
        raise ValueError("duplicate attributed GPA")
    return rows


def verify_compression(source):
    """Verify archived payload framing, codec round trips and sample metadata."""
    path = source / "compression.json"
    if not path.exists():
        return {"present": False}, {}
    record = json.loads(path.read_text())
    if record["codec"] != "zlib" or record["level"] != 1 or record["page_bytes"] != 4096:
        raise ValueError("unsupported recorded codec configuration")
    samples = record["samples"]
    rich = {r["gpa"]: r for r in load_rows(source / "resident-sample.jsonl.gz")}
    if len({s["gpa"] for s in samples}) != len(samples) or set(rich) != {s["gpa"] for s in samples}:
        raise ValueError("compression sample/rich-row identity mismatch")
    if record["sample_design"]["sample_pages"] != len(samples):
        raise ValueError("compression sample census mismatch")
    position = 0
    checks = {"payload_framing": True, "raw_hashes": True,
              "codec_recompression": True, "sample_row_categories": True,
              "stored_size_arithmetic": True}
    with (source / "compressed-sample-data.bin").open("rb") as stream:
        for sample in samples:
            size = sample["payload_length"]
            if sample["payload_offset"] != position or not 0 < size <= 8192:
                raise ValueError("noncontiguous/invalid compressed payload")
            packed = stream.read(size)
            if len(packed) != size or size != sample["compressed_bytes"]:
                raise ValueError("truncated compressed payload")
            obj = zlib.decompressobj()
            raw = obj.decompress(packed, 4097)
            if len(raw) != 4096 or not obj.eof or obj.unused_data or obj.unconsumed_tail:
                raise ValueError("invalid compressed page frame")
            if sample["raw_bytes"] != 4096 or hashlib.sha256(raw).hexdigest() != sample["raw_sha256"]:
                raise ValueError("sample raw payload hash mismatch")
            repacked = zlib.compress(raw, record["level"])
            if len(repacked) != size:
                raise ValueError("same-codec recompressed size differs")
            if not sample["roundtrip_ok"]:
                raise ValueError("source codec roundtrip failed")
            row = rich[sample["gpa"]]
            if sample["category"] != row["category"] or sample["process_names"] != row.get("process_names", []):
                raise ValueError("sample category/process differs from rich attribution")
            if sample["in_step_footprint"] != (row["access_status"] != "outside_measured_footprint"):
                raise ValueError("sample access-membership mismatch")
            overhead = record["metadata_bytes_per_compressed_page_assumption"]
            if sample["stored_bytes"] != min(4096, size + overhead) or sample["saved_bytes"] != max(0, 4096-size-overhead):
                raise ValueError("sample stored/saved byte arithmetic mismatch")
            position += size
        if stream.read(1):
            raise ValueError("unindexed trailing compressed payload")
    return {"present": True, "samples": len(samples), "payload_bytes": position,
            "codec": record["codec"], "level": record["level"],
            "method": "Archived payload decompression, raw SHA-256 and same-codec size verification; no new live payload reads"}, checks


def verify_resident_census(source):
    """Validate retained sample identities/counts without reading any payload."""
    census_path = source / "resident-census.json"
    if not census_path.exists():
        return {"present": False}, {}
    census = json.loads(census_path.read_text())
    population = census["resident_population"]
    design = census["sample_design"]
    selected = census["selection"]
    n = design["sample_pages"]
    total = design["population_pages"]
    requested = design["requested_pages"]
    if any(not isinstance(x, int) or x < 0 for x in (n, total, requested)):
        raise ValueError("invalid resident sample counts")
    if n != len(selected) or n != min(requested, total):
        raise ValueError("resident sample selection/count mismatch")
    if population["unique_nonzero_host_pfns"] != total or population["resident_gpa_pages"] < total:
        raise ValueError("resident population count mismatch")
    gpas = [x["gpa"] for x in selected]
    pfns = [x["host_pfn"] for x in selected]
    if len(set(gpas)) != n or len(set(pfns)) != n:
        raise ValueError("duplicate resident sample identity")
    if any(not isinstance(g, int) or g < 0 or g % 4096 for g in gpas):
        raise ValueError("invalid sampled GPA")
    if any(not isinstance(p, int) or p <= 0 for p in pfns):
        raise ValueError("invalid sampled host PFN")
    if any(not isinstance(x[k], int) or x[k] < 0
           for x in selected for k in ("host_kpagecount", "host_kpageflags")):
        raise ValueError("invalid sampled host metadata")
    rich_path = source / "resident-sample.jsonl.gz"
    rich = load_rows(rich_path) if rich_path.exists() else []
    if {x["gpa"] for x in rich} != set(gpas):
        raise ValueError("resident census/attribution identity mismatch")
    return {"present": True, "population_pages": total, "sample_pages": n,
            "payload_read": False,
            "scope": "Retained census internal consistency; does not independently prove random selection or live host residency"}, {
            "selection_counts": True, "selection_unique": True,
            "population_counts": True, "rich_row_identities": True}



def retained_reading(source, artifact_directory=None):
    """Resolve immutable legacy or companion inputs without a disposable-worker path."""
    source = Path(source)
    candidates = (source / "reading.json", source.parent / "reading.json",
                  source.parent / "phase.json")
    reading_path = next((path for path in candidates if path.is_file()), None)
    if reading_path is None:
        raise FileNotFoundError("no retained reading at " + ", ".join(map(str, candidates)))
    reading = json.loads(reading_path.read_text())
    mask_directory = None
    if "accessed_gpas" not in reading:
        from compact import expand_reading
        if artifact_directory is not None:
            mask_directory = Path(artifact_directory)
        elif source.parent.parent.name == "captures" and reading.get("vm_generation"):
            epoch = reading.get("source_epoch")
            if not isinstance(epoch, int) or epoch < 1:
                raise ValueError("compact companion reading requires source_epoch")
            mask_directory = source.parents[2] / ("epoch-%06d" % epoch)
        else:
            # Legacy phase artifacts retain their masks beside the reading.
            mask_directory = reading_path.parent
        reading = expand_reading(reading, artifact_directory=mask_directory)
    return reading, reading_path, mask_directory

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--step", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--bootstrap", type=Path)
    parser.add_argument("--artifact-directory", type=Path,
                        help="explicit retained compact-mask directory for a nonstandard archive layout")
    args = parser.parse_args()
    source = args.step.resolve()
    bootstrap_path = args.bootstrap or source.parent / "bootstrap.json"
    bootstrap = json.loads(bootstrap_path.read_text())
    reading, reading_path, mask_directory = retained_reading(source, args.artifact_directory)
    frozen = FrozenMemory(source / "metadata-index.json", source / "metadata-pages.bin.gz")
    decoder = GuestDecoder.__new__(GuestDecoder)
    decoder.pid = reading["vmm_pid"]
    decoder.vm_id = reading["vm_instance"]
    decoder.slots = []
    decoder.config = dict(bootstrap.get("attribution_config", {}))
    compression_path = source / "compression.json"
    if compression_path.exists():
        compression_record = json.loads(compression_path.read_text())
        decoder.config["replay_sample_gpas"] = [s["gpa"] for s in compression_record["samples"]]
    elif (source / "resident-census.json").exists():
        census_record = json.loads((source / "resident-census.json").read_text())
        decoder.config["replay_sample_gpas"] = [s["gpa"] for s in census_record["selection"]]
    elif decoder.config.get("enable_resident_sampling"):
        decoder.config["replay_sample_gpas"] = []
    decoder.mem = frozen
    decoder.meta = bootstrap["layout"]
    decoder.l = decoder.meta["layout"]
    decoder.image = SimpleNamespace(symbols=decoder.meta["symbols"])
    decoder.a = bootstrap["anchors"]
    decoder.cache = {}
    decoder.errors = []
    decoder.info = dict(line.split("=", 1) for line in decoder.a["vmcoreinfo"].splitlines()
                        if "=" in line)
    args.output.mkdir(parents=True, exist_ok=False)
    result = decoder.capture(reading, args.output)
    checks = {}
    differences = {}
    before = load_rows(source / "pages.jsonl.gz")
    after = load_rows(args.output / "pages.jsonl.gz")
    checks["per_page_rows_exact"] = before == after
    if before != after:
        a = {r["gpa"]: r for r in before}
        b = {r["gpa"]: r for r in after}
        changed = [g for g in sorted(a.keys() | b.keys()) if a.get(g) != b.get(g)]
        differences["pages"] = {"count": len(changed), "first_gpas": changed[:20]}
    for name in ("processes.json", "vmas.json", "owners.json", "files.json", "file-namespaces.json"):
        if not (source / name).exists():
            if (args.output / name).exists():
                checks[name + "_unexpected"] = False
            continue
        left = json.loads((source / name).read_text())
        right = json.loads((args.output / name).read_text())
        checks[name + "_exact"] = left == right
    original_summary = json.loads((source / "summary.json").read_text())
    for key, value in original_summary.items():
        if key.endswith("_ns"):
            continue
        checks["summary_" + key] = value == result.get(key)
    if (source / "resident-sample.jsonl.gz").exists():
        checks["resident_sample_rows_exact"] = load_rows(source / "resident-sample.jsonl.gz") == load_rows(args.output / "resident-sample.jsonl.gz")
    census, census_checks = verify_resident_census(source)
    checks.update({"resident_census_" + k: v for k, v in census_checks.items()})
    (args.output / "resident-census-validation.json").write_text(json.dumps(census, indent=2) + "\n")
    compression, codec_checks = verify_compression(source)
    checks.update({"compression_" + k: v for k, v in codec_checks.items()})
    (args.output / "compression-validation.json").write_text(json.dumps(compression, indent=2) + "\n")
    paths = [bootstrap_path, reading_path, source / "metadata-index.json",
             source / "metadata-pages.bin.gz", source / "pages.jsonl.gz",
             source / "processes.json", source / "vmas.json", source / "summary.json",
             Path(__file__), Path(__file__).with_name("decoder.py"),
             Path(__file__).with_name("memory.py")]
    paths += [p for p in (source / "resident-census.json", source / "compression.json", source / "compressed-sample-data.bin",
                          source / "resident-sample.jsonl.gz", source / "owners.json",
                          source / "files.json", source / "file-namespaces.json",
                          Path(__file__).with_name("owners.py"), Path(__file__).with_name("file_names.py"),
                          Path(__file__).with_name("compression.py")) if p.exists()]
    if mask_directory is not None:
        paths.append(Path(__file__).with_name("compact.py"))
        raw = json.loads(reading_path.read_text())
        paths += [mask_directory / mask["file"] for region in raw["regions"]
                  for mask in region["masks"].values()]
    report = {
        "schema": "aries.guest-attribution-replay.v2",
        "compression_validation": compression,
        "resident_census_validation": census,
        "passed": all(checks.values()), "checks": checks, "differences": differences,
        "source_step": str(source), "live_memory_access": False,
        "reading_path": str(reading_path),
        "compact_mask_directory": str(mask_directory) if mask_directory is not None else None,
        "retained_metadata_pages": len(frozen.pages),
        "replayed_metadata_pages": len(frozen.requested),
        "input_sha256": {str(p): digest(p) for p in paths},
        "scope": "Exact deterministic replay of decoder output from retained metadata. "
                 "This proves capture sufficiency for this decoder, not independent semantic correctness.",
    }
    (args.output / "replay-validation.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"passed": report["passed"], "checks": checks, "differences": differences}))
    if not report["passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
