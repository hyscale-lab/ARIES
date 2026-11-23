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


def opencv_profile(receipt):
    ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
    profile = ready.get("video_opencv_ipp")
    if profile is None:
        if receipt.get("video_opencv_runtime"):
            raise ValueError("OpenCV environment readback lacks a validation profile")
        return None
    if profile != "disabled" or receipt["trial_id"] not in {
        "aa067203-b539-48d0-9c99-2128f941c9d0", "020f911b-467e-49ad-8113-bb725bee1d4b"
    }:
        raise ValueError("unexpected source-specific OpenCV environment")
    validate_runtime(receipt["video_opencv_runtime"])
    return profile


def validate_runtime(events):
    if [e["phase"] for e in events] != ["before_snapshot", "after_restore"]:
        raise ValueError("missing OpenCV deployment readback across restore")
    observed = [e["runtime"] for e in events]
    if observed[0] != observed[1]:
        raise ValueError("restored OpenCV execution profile differs")
    r = observed[0]
    if (r["profile"] != "disabled" or r["ipp_enabled"] is not False or r["ipp_inexact"] is not False
            or (r["opencv"], r["numpy"], r["python"], r["video_backend"]) != ("5.0.0", "2.5.2", "3.13.7", "FFMPEG")
            or not r["cpu_features"] or not re.fullmatch("[a-f0-9]{64}", r["opencv_build_sha256"])):
        raise ValueError("native OpenCV profile readback differs from reviewed software")
    return r


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
    if profile := opencv_profile(receipt):
        args.extend(["-video-opencv-ipp", profile])
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
    runtime_events = [e for e in events if e["event"] == "video_opencv_runtime"]
    if opencv_profile(receipt):
        current = validate_runtime(runtime_events)
        reference = validate_runtime(receipt["video_opencv_runtime"])
        if current["opencv_build_sha256"] != reference["opencv_build_sha256"]:
            raise ValueError("OpenCV build identity differs from native validation")
        result["video_opencv_runtime"] = current
    elif runtime_events:
        raise ValueError("unexpected OpenCV execution profile")
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
