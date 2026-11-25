"""Identify observed native call batches and outstanding process handles."""
from itertools import groupby


def identify(events, complete=True, relaxed_status=False, ending_handles=(), ending_retained_handles=()):
    calls = [e for e in events if e["event"] == "tool_dispatch"]
    replies = [e for e in events if e["event"] == "tool_reply"]
    issued = {e["sequence"]: e for e in calls}
    returned = {e["sequence"]: e for e in replies}
    if len(issued) != len(calls) or len(returned) != len(replies):
        raise ValueError("duplicate native tool occurrence")
    if complete and (not calls or issued.keys() != returned.keys()):
        raise ValueError("incomplete dispatch or reply set")
    groups = {}
    for step, group in groupby(calls, lambda e: e["step_id"]):
        members = list(group)
        if step in groups or (complete and any((e.get("parallel_size") or 1) != len(members) for e in members)):
            raise ValueError("ambiguous or incomplete native batch")
        groups[step] = {"member_sequences": [e["sequence"] for e in members],
                        "member_operations": [e.get("operation", "Exec") for e in members]}
    active = {}
    retained = {}
    unmatched = []
    for event in events:
        kind = event["event"]
        if (relaxed_status and kind == "diagnostic_process_observation" and
                event.get("status_basis") == "additional_join_after_source_poll"):
            handle = event["source_process_session_id"]
            if handle not in active or event["exit_code"] < 0:
                raise ValueError("invalid terminal join for source process")
            event["start_sequence"] = active.pop(handle)["sequence"]
        if kind not in {"tool_dispatch", "tool_reply"}:
            if active and kind in {"wait_start", "wait_end", "source_event"}:
                event["background_handles_possibly_running"] = sorted(active)
            continue
        step = event["step_id"]
        group = groups.get(step)
        if kind == "tool_dispatch":
            group.setdefault("background_handles_before", sorted(active))
            continue
        sequence = event["sequence"]
        call = issued.get(sequence)
        if call is None or call["step_id"] != step:
            if complete:
                raise ValueError("unbound live tool reply")
            event["process_transition"] = "reply_without_matching_dispatch"
            unmatched.append(sequence)
            continue
        success = event["exit_code"] == event["expected_exit_code"]
        if complete and not success and not relaxed_status:
            raise ValueError("divergent live process status")
        operation = event.get("operation", "Exec")
        handle = event.get("source_process_session_id")
        if operation in {"StartProcess", "PollProcess"}:
            if not handle:
                raise ValueError("asynchronous tool reply missing its source handle")
            if operation == "StartProcess" and event["exit_code"] == -1:
                if handle in active:
                    raise ValueError("duplicate open source process handle")
                active[handle] = event
                event["process_transition"] = "process_started"
            elif operation == "StartProcess":
                event["process_transition"] = "completed_before_source_yield"
                if relaxed_status:
                    retained[handle] = event
            elif relaxed_status and handle in retained:
                terminal = retained[handle]
                if event["exit_code"] != terminal["exit_code"] or event.get("status_basis") != "retained_terminal_status_empty_poll":
                    raise ValueError("retained terminal poll differs from observed terminal status")
                event["process_transition"] = "retained_terminal_status_empty_poll"
                event["terminal_sequence"] = terminal["sequence"]
                if event["expected_exit_code"] != -1:
                    retained.pop(handle)
            elif handle not in active:
                event["process_transition"] = "poll_without_observed_start"
                unmatched.append(sequence)
            elif event["exit_code"] == -1:
                event["process_transition"] = "polled_still_running"
            else:
                event["process_transition"] = "terminal_poll"
                event["start_sequence"] = active.pop(handle)["sequence"]
                if relaxed_status and event["expected_exit_code"] == -1:
                    retained[handle] = event
        group["background_handles_after"] = sorted(active)
    for event in active.values():
        event["terminal_poll_observed"] = False
    if complete and (set(active) != set(ending_handles) or unmatched or set(retained) != set(ending_retained_handles)):
        raise ValueError("unmatched native start or poll")
    return groups, {"open_handles": sorted(active), "unmatched_reply_sequences": unmatched}


def assign(window_kind, step, groups):
    if window_kind == "final":
        return window_kind, {"attribution": "final_inter_event_gap"}
    group = groups.get(step)
    if not group:
        raise ValueError("freeze boundary has no matching source step")
    before = group["background_handles_before"]
    if window_kind == "wait":
        if before:
            return "wait-with-background", {"attribution": "inter_event_gap_with_possible_background_execution",
                                           "background_handles_possibly_running": before}
        return window_kind, {"attribution": "inter_event_gap"}
    details = dict(group)
    if len(group["member_sequences"]) > 1:
        details["attribution"] = "parallel_batch_union"
        return "tool-batch", details
    details["attribution"] = "single_call_with_possible_background_execution" if before else "single_call"
    return window_kind, details
