#!/usr/bin/env python3
"""Turns an aries-replay recordings file into a readable Markdown report.

    python3 scripts/microbench/decode_recordings.py RECORDINGS.jsonl [-o OUT.md] [--full]

Each recording holds the model's reply as the raw chunks it streamed, and,
for recordings made since requests were kept, the request the harness sent.
The report groups recordings by conversation (the task's first user message)
and lists each conversation's recordings in the order they were made, labelled
with the run they belong to: a task recorded with --warm runs twice on one
harness, so each of its model calls is recorded once in the cold run and once
in the warm run. A third or later run is labelled by its number. For each
recording it shows:

  - when it was recorded, the time to the first byte and to the last;
  - what the harness sent since the model's previous reply (tool results, or
    the user's message), and how many tools it offered;
  - the reply: the model's reasoning, its text, and the tool calls it made, or
    the provider's error;
  - token usage, when the provider reported it.

--full also includes each request's complete message list. Standard library
only.
"""

import argparse
import base64
import datetime
import json
import re
import sys
from collections import defaultdict

TRIM = 1500


def text_of(content):
    """A message's content as text: a string, or a list of text parts."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(part.get("text", "") for part in content if isinstance(part, dict))
    return "" if content is None else json.dumps(content)


def trimmed(text, limit=TRIM):
    text = text.strip()
    if len(text) <= limit:
        return text
    return text[:limit] + "\n… (%d more characters)" % (len(text) - limit)


def block(text, language="text", indent=""):
    """A fenced code block whose fence is longer than any run of backticks in
    the text, so a task's own code blocks cannot close it early."""
    longest = max((len(run) for run in re.findall(r"`+", text)), default=0)
    fence = "`" * max(3, longest + 1)
    return [indent + fence + language] + [indent + line for line in text.splitlines() or [""]] + [indent + fence]


def cell(text):
    """Text for one table cell: one line, with its pipes and backticks escaped."""
    return " ".join(text.split()).replace("|", "\\|").replace("`", "\\`")


def body_of(recording):
    return b"".join(base64.b64decode(chunk.get("data") or "") for chunk in recording.get("chunks") or [])


def decode_reply(recording):
    """Reassembles a streamed (server-sent events) or whole JSON reply."""
    reply = {"reasoning": "", "content": "", "tool_calls": {}, "finish": None, "usage": None}
    raw = body_of(recording).decode("utf-8", "replace")
    if "text/event-stream" in (recording.get("content_type") or ""):
        events = [line[5:].strip() for line in raw.splitlines() if line.startswith("data:")]
        chunks = [json.loads(event) for event in events if event and event != "[DONE]"]
    else:
        whole = json.loads(raw) if raw.strip() else {}
        message = ((whole.get("choices") or [{}])[0]).get("message") or {}
        chunks = [{"choices": [{"delta": message, "finish_reason": (whole.get("choices") or [{}])[0].get("finish_reason")}], "usage": whole.get("usage")}]
    for chunk in chunks:
        if chunk.get("usage"):
            reply["usage"] = chunk["usage"]
        for choice in chunk.get("choices") or []:
            delta = choice.get("delta") or {}
            reply["reasoning"] += delta.get("reasoning_content") or ""
            reply["content"] += text_of(delta.get("content"))
            for call in delta.get("tool_calls") or []:
                slot = reply["tool_calls"].setdefault(call.get("index", len(reply["tool_calls"])), {"name": "", "arguments": ""})
                function = call.get("function") or {}
                slot["name"] += function.get("name") or ""
                slot["arguments"] += function.get("arguments") or ""
            if choice.get("finish_reason"):
                reply["finish"] = choice["finish_reason"]
    return reply


def new_messages(messages):
    """What the harness sent since the model's last reply. A first request
    shows its user message, and its system prompt when that is short, as for
    a background request such as a session title."""
    last = max((i for i, message in enumerate(messages) if message.get("role") == "assistant"), default=-1)
    if last >= 0:
        return messages[last + 1:]
    return [m for m in messages if m.get("role") == "user"
            or (m.get("role") == "system" and len(text_of(m.get("content"))) < 2000)]


RUN_NAMES = {1: "Cold", 2: "Warm"}


def label_runs(group):
    """Names the run of each of a conversation's recordings, which are in
    recorded order. The agent's own calls offer tools, and a run starts at
    each of its first turns: the first run is the cold one and the second the
    warm one. A background call, which offers none, belongs to the run whose
    first agent call is nearest in time: a harness sends it beside that call,
    sometimes just before it."""
    starts = []
    for recording in group:
        if recording["key"]["tools"]:
            if recording["key"]["turn"] == 0:
                starts.append(recording.get("recorded_at", ""))
            recording["_number"] = max(len(starts), 1)
    seconds = lambda text: parse_time(text) if text else 0.0
    for recording in group:
        if not recording["key"]["tools"]:
            moment = seconds(recording.get("recorded_at"))
            recording["_number"] = min(range(len(starts)), key=lambda i: abs(seconds(starts[i]) - moment)) + 1 if starts else 1
        recording["_run"] = RUN_NAMES.get(recording["_number"], "Run %d" % recording["_number"])


def parse_time(text):
    # Go writes nanoseconds; Python keeps microseconds.
    text = text.replace("Z", "+00:00")
    if "." in text:
        head, rest = text.split(".", 1)
        digits = rest[:len(rest) - len(rest.lstrip("0123456789"))]
        text = "%s.%s%s" % (head, digits[:6].ljust(6, "0"), rest[len(digits):])
    return datetime.datetime.fromisoformat(text).timestamp()


def calls(group, run):
    """A run's model calls: the agent's, and background ones offering no tools."""
    agent = sum(1 for r in group if r["_run"] == run and r["key"]["tools"])
    background = sum(1 for r in group if r["_run"] == run and not r["key"]["tools"])
    return "%d%s" % (agent, " (+%d background)" % background if background else "")


def first_user(recording):
    request = recording.get("request")
    if not request:
        return None
    for message in request.get("messages") or []:
        if message.get("role") == "user":
            return text_of(message.get("content"))
    return None


def pretty_arguments(arguments):
    try:
        return json.dumps(json.loads(arguments), indent=2, ensure_ascii=False)
    except ValueError:
        return arguments


def render(recordings, full):
    conversations = defaultdict(list)
    for recording in recordings:
        conversations[recording["key"]["conversation"]].append(recording)
    order = sorted(conversations, key=lambda c: min(r.get("recorded_at", "") for r in conversations[c]))
    out = ["# Recorded model calls", ""]
    out.append("%d recordings in %d conversations. Times are from when aries-replay received the request." % (len(recordings), len(order)))
    for conversation in order:
        conversations[conversation].sort(key=lambda r: r.get("recorded_at", ""))
        label_runs(conversations[conversation])
        conversations[conversation].sort(key=lambda r: (r["_number"], r.get("recorded_at", "")))
    numbers = sorted({r["_number"] for r in recordings})
    runs = [RUN_NAMES.get(n, "Run %d" % n) for n in numbers]
    out += ["", "Model calls recorded per run:", "", "| Conversation | First user message | %s |" % " | ".join(runs), "|---|---|" + "---|" * len(runs)]
    for conversation in order:
        group = conversations[conversation]
        first = next((text for text in map(first_user, group) if text), "(request not stored)")
        out.append("| `%s` | %s | %s |" % (
            conversation, cell(" ".join(first.split())[:80]), " | ".join(calls(group, run) for run in runs)))
    for conversation in order:
        group = conversations[conversation]
        first = next((text for text in map(first_user, group) if text), None)
        out += ["", "## Conversation `%s`" % conversation, ""]
        if first is not None:
            out += ["**First user message:**", ""] + block(trimmed(first, 4000))
        for recording in group:
            key = recording["key"]
            chunks = recording.get("chunks") or []
            first_byte = chunks[0]["offset_ms"] if chunks else 0
            last_byte = chunks[-1]["offset_ms"] if chunks else 0
            out += ["", "### %s run, turn %d%s" % (recording["_run"], key["turn"], "" if key["tools"] else ", background request (offers no tools)"), ""]
            out.append("Recorded %s · status %s · first byte %.0f ms · last byte %.0f ms · %d chunks · %d tools offered" % (
                recording.get("recorded_at", "?"), recording.get("status"), first_byte, last_byte, len(chunks), key["tools"]))
            request = recording.get("request")
            if request:
                out += ["", "**Sent since the last reply:**", ""]
                for message in new_messages(request.get("messages") or []):
                    role = message.get("role")
                    label = role + (" (%s)" % message["tool_call_id"] if message.get("tool_call_id") else "")
                    out += ["- *%s*:" % label, ""] + block(trimmed(text_of(message.get("content"))), indent="  ")
                if full:
                    out += ["", "<details><summary>Full request (%d messages)</summary>" % len(request.get("messages") or []), ""]
                    out += block(json.dumps(request.get("messages"), indent=2, ensure_ascii=False), "json") + ["", "</details>"]
            else:
                out += ["", "*The request was not stored with this recording.*"]
            if recording.get("status") != 200:
                error = body_of(recording).decode("utf-8", "replace")
                try:
                    error = json.loads(error)["error"]["message"]
                except (ValueError, KeyError, TypeError):
                    pass
                out += ["", "**Error** (replay does not serve it): %s" % trimmed(error, 500)]
                continue
            reply = decode_reply(recording)
            out += ["", "**Reply** (finish: %s):" % reply["finish"], ""]
            if reply["reasoning"]:
                out += ["> " + line for line in trimmed(reply["reasoning"]).splitlines()]
                out.append("")
            if reply["content"]:
                out += block(trimmed(reply["content"]))
            for index in sorted(reply["tool_calls"]):
                call = reply["tool_calls"][index]
                out += ["", "Tool call `%s`:" % call["name"], ""] + block(pretty_arguments(call["arguments"]), "json")
            if reply["usage"]:
                usage = reply["usage"]
                out.append("")
                out.append("Tokens: %s prompt, %s completion." % (usage.get("prompt_tokens"), usage.get("completion_tokens")))
    return "\n".join(out) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("recordings")
    parser.add_argument("-o", "--out", help="write here instead of standard output")
    parser.add_argument("--full", action="store_true", help="include each request's complete message list")
    args = parser.parse_args()
    with open(args.recordings) as handle:
        recordings = [json.loads(line) for line in handle if line.strip()]
    report = render(recordings, args.full)
    if args.out:
        with open(args.out, "w") as handle:
            handle.write(report)
        print("wrote %d recordings to %s" % (len(recordings), args.out), file=sys.stderr)
    else:
        sys.stdout.write(report)


if __name__ == "__main__":
    main()
