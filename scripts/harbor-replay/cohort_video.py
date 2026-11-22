"""Check restored video frames, codec wheel bytes and the source output."""
import json
import re

from cohort_dataset import network_args
from cohort_image_circuit import IMAGE_HELPER, digest, verify as verify_guest

TASK = "video-processing"
WHEEL_HASHES = {
    "numpy": "29b86ff8a6cc556b47ec6b64b194815cc80e6bf5eedcc6cddfd65318cb0b4eee",
    "opencv": "8427dcb0561dc3ba32f3771c627f52b87c29932c265bea28ffcb54804c3c3fec",
}


def replay_args(asset, receipt, cni):
    args = network_args(cni, "zbvideo")
    helper = asset / "aries-harbor-image"
    if digest(helper) != IMAGE_HELPER:
        raise ValueError("incorrect guest image codec")
    args.extend(["-image-helper", str(helper)])
    ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
    if sha := ready.get("debian_patch_archive_sha256"):
        path = asset / "circuit-patch-debs.tar.gz"
        if digest(path) != sha:
            raise ValueError("GNU patch archive differs from the validated guest")
        args.extend(["-os-tools", str(path)])
    return args


def verify(run, receipt):
    result = verify_guest(run, receipt)
    events = [json.loads(line) for line in (run / "events.jsonl").read_bytes().splitlines()]
    helpers = [e["guest_sha256"] for e in events if e["event"] == "guest_prepared" and e["helper"] == "aries-harbor-image"]
    if helpers != [IMAGE_HELPER] or result["wire_identical_images"] == 0:
        raise ValueError("video guest codec or original image observations missing")
    ready, = [e["state"]["checks"] for e in events if e["event"] == "source_task_ready"]
    if ("ready guest 5.0.0 2.5.2" not in ready
            or any("--hash=sha256:" + sha not in ready for sha in WHEEL_HASHES.values())):
        raise ValueError("OpenCV and NumPy wheel hashes and version evidence differ")
    networks = [e for e in events if e["event"] == "validated_guest_network"]
    if [e["phase"] for e in networks] != ["before_snapshot", "after_restore"]:
        raise ValueError("missing network evidence across restore")
    if any(e["network"]["pypi_status"] != 200 or not e["network"]["pypi_addresses"] for e in networks):
        raise ValueError("guest could not reach the original package index")
    final = result["final_checks"].splitlines()
    if not re.fullmatch(r"[a-f0-9]{64}  output\.toml", final[0]) or "example_video.mp4: OK" not in final:
        raise ValueError("final source video/output verification missing")
    external = [e for e in events if e["event"] == "source_event" and e.get("source_kind") == "web_search_call"]
    keys = ("step_id", "source_call_id", "rollout_line", "basis")
    if [tuple(e[k] for k in keys) for e in external] != [
        tuple(e[k] for k in keys) for e in receipt.get("external_completion_events", [])
    ]:
        raise ValueError("source external completion differs")
    return dict(**result, guest_codec_sha256=IMAGE_HELPER, wheel_hashes=WHEEL_HASHES,
                external_completions=len(external))
