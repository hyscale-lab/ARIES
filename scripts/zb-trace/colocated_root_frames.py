"""Verify at one observation whether two restored DAX roots share mapped physical frames."""
import argparse
import json
from pathlib import Path
import time

from shared_root import observe


def check(period):
    roles = {}
    begin = time.monotonic_ns()
    for role in ("a", "b"):
        path = period/role
        receipt = json.loads((path/"restored-backing.json").read_text())
        root = receipt["root"]
        observed = observe(path/"samples.jsonl", Path(root["opened_path"]).name)
        if observed is None:
            raise ValueError("missing live restored VMM or mapped immutable root")
        info, frames = observed
        if info["file_id"] != (root["device"], root["inode"]) or info["ticks"] != receipt["start_ticks"]:
            raise ValueError("pagemap observation differs from the restored backing receipt")
        roles[role] = info, frames
    (a, x), (b, y) = roles.values()
    if a["file_id"] != b["file_id"]:
        raise ValueError("the two restored VMMs use different root inodes")
    common = x.keys() & y.keys()
    return dict(period=period.name, begin_ns=begin, end_ns=time.monotonic_ns(),
                roles={key: value[0] for key, value in roles.items()},
                same_offset_pages=len(common), same_offset_same_frame_pages=sum(x[i] == y[i] for i in common),
                distinct_mapped_frames=len(set(x.values()) | set(y.values())))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("period", type=Path)
    args = parser.parse_args()
    print(json.dumps(check(args.period)))
