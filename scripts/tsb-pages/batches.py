"""One VM page-idle observation per complete native LLM tool-request batch."""
import collections
import time

SCHEMA = "aries.tsb.tool-pages.llm-batch.v1"
VALIDITY_SCOPE = "expected_batch_registration_and_result_completion_only"
SCOPE = ("partial whole-VM temporal envelope of one LLM reasoning step: "
         "native assistant tool request through all expected tool results ready for the LLM; "
         "includes native harness preparation, sequential/parallel calls and background activity")
UNCERTAINTY = [
    "finite arm/scan operations are non-atomic while the VMM keeps running",
    "endpoint residency/PFN checks can miss transient accesses and mapping changes",
    "unchanged idle bits can miss accesses through KVM translations",
    "native result-persistence delivery can lag actual tool execution completion",
    "shared LLM-step page sets do not identify which native call caused an access",
]


def identity(key):
    return dict(zip(("session_id", "turn_id", "tool_call_id"), key))


class BatchTracker:
    def __init__(self, observer, save_group, clock=time.monotonic_ns, wall_clock=time.time_ns):
        self.observer = observer
        self.save_group = save_group
        self.clock = clock
        self.wall_clock = wall_clock
        self.active = {}
        self.pending = None
        self.finished_batches = set()
        self.invalid = False
        self.last_ack = {}

    def acknowledgement(self, batch, collection):
        return {
            "ok": True, "batch_id": batch["metadata"]["batch_id"],
            "llm_step_id": batch["metadata"]["llm_step_id"],
            "group_id": batch["metadata"]["batch_id"], "collection": collection,
            "remaining_expected_tool_call_ids": [
                call for call in batch["expected"] if call not in batch["completed"]],
            "remaining_active_tool_call_ids": sorted(key[2] for key in self.active),
            "granularity": "llm_reasoning_step", "coverage_complete": False,
            "measurement_valid_scope": VALIDITY_SCOPE,
        }

    def register(self, metadata):
        batch_id = metadata.get("batch_id")
        llm_step_id = metadata.get("llm_step_id")
        if not isinstance(batch_id, str) or not batch_id or llm_step_id != batch_id:
            raise ValueError("matching nonempty batch_id and llm_step_id required")
        session = metadata.get("session_id"), metadata.get("turn_id")
        if not all(isinstance(value, str) and value for value in session):
            raise ValueError("batch session/turn IDs required")
        batch_key = session + (batch_id,)
        if batch_key in self.finished_batches:
            raise ValueError("duplicate completed batch registration")
        if self.pending is not None:
            raise ValueError("previous LLM batch still awaits expected results; refusing a bitmap reset")
        tools = metadata.get("requested_tools")
        if not isinstance(tools, list) or not tools:
            raise ValueError("nonempty native requested_tools required")
        expected = {}
        for tool in tools:
            if not isinstance(tool, dict):
                raise ValueError("native tool identity must be an object")
            call, name = tool.get("tool_call_id"), tool.get("tool_name")
            if not isinstance(call, str) or not call or not isinstance(name, str) or not name:
                raise ValueError("native tool call ID/name required")
            if call in expected:
                raise ValueError("duplicate expected native tool call ID")
            expected[call] = {
                "tool_call_id": call, "tool_name": name, "begin_observed": False,
                "begin_monotonic_ns": None, "end_monotonic_ns": None, "is_error": None,
            }
        if metadata.get("expected_tool_call_ids") != list(expected):
            raise ValueError("expected IDs/order differ from requested native tools")
        arm = self.observer.arm(batch_id)
        batch = {
            "metadata": dict(metadata), "expected": expected, "completed": set(),
            "completed_order": [], "arm": arm, "concurrency_peak": 0, "key": batch_key,
        }
        self.pending = batch
        self.last_ack = self.acknowledgement(batch, "armed")
        self.save_group({
            **metadata, "schema": "aries.tsb.llm-tool-batch.v1", "event": "batch_begin",
            "group_id": batch_id, "observer_arm": arm, "host_monotonic_ns": arm["arm_end_ns"],
            "coverage_complete": False, "measurement_valid_scope": VALIDITY_SCOPE,
            "observer_ack": self.last_ack, "scope": SCOPE,
        })
        return self.last_ack

    def member(self, key, metadata):
        batch = self.pending
        if batch is None:
            raise ValueError("native call/result has no registered LLM batch")
        declared = batch["metadata"]
        if key[:2] != batch["key"][:2] or metadata.get("batch_id") != declared["batch_id"]:
            raise ValueError("native call/result belongs to another LLM batch")
        if metadata.get("llm_step_id") != declared["llm_step_id"]:
            raise ValueError("native call/result has a different LLM step ID")
        if key[2] not in batch["expected"]:
            raise ValueError("native call/result ID was not requested by the registered assistant message")
        if key[2] in batch["completed"]:
            raise ValueError("duplicate native result or admission after completion")
        if metadata.get("tool_name") != batch["expected"][key[2]]["tool_name"]:
            raise ValueError("native tool name differs from the registered assistant message")
        declared_session = declared.get("native_session_key")
        native_session = metadata.get("native_session_key")
        if declared_session and native_session and declared_session != native_session:
            raise ValueError("native session differs from the registered assistant message")
        return batch, batch["expected"][key[2]]

    def begin(self, key, metadata):
        batch, member = self.member(key, metadata)
        if key in self.active or member["begin_observed"]:
            raise ValueError("duplicate native call begin")
        member["begin_observed"] = True
        member["begin_monotonic_ns"] = self.clock()
        member["observed_tool_name"] = metadata.get("tool_name")
        self.active[key] = member
        batch["concurrency_peak"] = max(batch["concurrency_peak"], len(self.active))
        self.last_ack = self.acknowledgement(batch, "pending")
        return {
            "event": "step_begin", **metadata, "group_id": batch["metadata"]["batch_id"],
            "host_monotonic_ns": member["begin_monotonic_ns"],
            "observer_ack": self.last_ack, "coverage_complete": False,
            "measurement_valid_scope": VALIDITY_SCOPE,
        }

    def end(self, key, metadata, ended_ns=None):
        batch, member = self.member(key, metadata)
        ended_ns = self.clock() if ended_ns is None else ended_ns
        last = len(batch["completed"]) + 1 == len(batch["expected"])
        # Completion is driven by the LLM response's expected IDs, never by
        # active_count. Sequential gaps and immediate errors remain in one arm.
        reading = self.observer.collect() if last else None
        collected_mono = self.clock() if last else None
        collected_wall = self.wall_clock() if last else None
        member["end_monotonic_ns"] = ended_ns
        member["is_error"] = metadata.get("is_error", False)
        batch["completed"].add(key[2])
        batch["completed_order"].append(key[2])
        self.active.pop(key, None)
        self.last_ack = self.acknowledgement(batch, "collected" if last else "pending")
        if last:
            members = list(batch["expected"].values())
            expected_ids = list(batch["expected"])
            declared = batch["metadata"]
            reading = dict(reading)
            reading.pop("tool_call_id", None)
            uncertainty = list(reading.get("batch_uncertainty", UNCERTAINTY))
            if reading.get("boundary_pause"):
                if "finite arm/scan operations are non-atomic while the VMM keeps running" in uncertainty:
                    uncertainty.remove("finite arm/scan operations are non-atomic while the VMM keeps running")
                uncertainty.append("guest vCPUs paused at boundaries; in-flight device writes are not proven drained")
            reading.update({
                "schema": (reading.get("schema") if reading.get("schema") == "aries.vmm-tracer.compact.v1"
                           else SCHEMA), "batch_id": declared["batch_id"],
                "llm_step_id": declared["llm_step_id"], "group_id": declared["batch_id"],
                "expected_tool_call_ids": expected_ids, "member_tool_call_ids": expected_ids,
                "group_members": members, "concurrency_peak": batch["concurrency_peak"],
                "coverage_complete": False, "coverage_scope": SCOPE,
                "coverage_uncertainty_reasons": uncertainty,
                "measurement_valid_scope": VALIDITY_SCOPE,
                "transition_gaps": [], "transition_gap_envelope_ns": 0,
                "interior_bitmap_resets": 0,
                "observation_unit": "llm_reasoning_step",
                "gap_counting_policy": "one continuous armed LLM batch; no interior bitmap resets",
                "caveats": list(dict.fromkeys(reading.get("caveats", []) + uncertainty)),
            })
            self.save_group({
                **declared, "schema": "aries.tsb.llm-tool-batch.v1", "event": "group_end",
                "group_id": declared["batch_id"], "step_id": declared["llm_step_id"],
                "tool_name": "LLM tool-request batch", "member_tool_call_ids": expected_ids,
                "completed_tool_call_ids": batch["completed_order"],
                "group_members": members, "concurrency_peak": batch["concurrency_peak"],
                "collection_complete_monotonic_ns": collected_mono,
                "collection_complete_wall_time_ns": collected_wall,
                "result": {"begin_monotonic_ns": batch["arm"]["arm_end_ns"],
                           "end_monotonic_ns": ended_ns},
                "metadata": {"is_error": any(value["is_error"] for value in members)},
                "reading": reading, "measurement_valid": not self.invalid,
                "measurement_valid_scope": VALIDITY_SCOPE,
                "coverage_complete": False, "scope": SCOPE, "observer_ack": self.last_ack,
            })
            self.finished_batches.add(batch["key"])
            self.pending = None
        return {
            "event": "step_end", **identity(key), "step_id": key[2],
            "batch_id": metadata["batch_id"], "llm_step_id": metadata["llm_step_id"],
            "group_id": metadata["batch_id"], "tool_name": member["tool_name"],
            "begin_observed": member["begin_observed"], "metadata": metadata,
            "observer_ack": self.last_ack,
            "result": {"begin_monotonic_ns": member["begin_monotonic_ns"],
                       "end_monotonic_ns": ended_ns},
            "measurement_valid": not self.invalid,
            "measurement_valid_scope": VALIDITY_SCOPE, "coverage_complete": False,
            "scope": SCOPE,
        }
