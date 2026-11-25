# Harbor Codex evidence and ARIES replay

The inspected Terminal-Bench 2.1 trial had three shell commands and one
add-file change. This map describes the evidence in that trial and how the
current converter uses it. It does not establish compatibility for all Harbor
workloads. See the [replay design](../design/replayer.md#94-harbor-conversion-and-replay)
and [operator steps](../quick-start.md#replay-a-harbor-codex-recording).

| Evidence | Replay use | Limit |
| --- | --- | --- |
| Rollout request and return times | Schedule from Codex request offsets. The inspected offsets were 0, 36.950, 38.247, and 44.214 seconds. | The transcript has no timestamps. The first trajectory step precedes its rollout request by 735 ms. Neither source measures sandbox admission. |
| Shell transcript and rollout request | Preserve the script, Bash login setting, and workdir. A command and its optional empty-input wait become one `Exec`. | The effective environment was not recorded. A conditional wait need not have run. No separate poll or interactive process session is replayed. |
| Successful add-file request and result | Use the recorded path and complete new content for `WriteFile`. | The trace does not contain a native response frame. Check RPC success and the returned write count. |
| Shell exit code and decoded combined output | Check the exit code and compare the combined UTF-8 byte count against the sum of stdout and stderr bytes. | Do not count JSON escaping, status text, or timing headers. Keep payload whitespace. Native frame sizes and digests are not available. |
| Agent phase end | Set the final wait from the last tool return to `agent_execution.finished_at`. | A verifier or job finish is not the agent phase end. The source end does not establish native caller stop. |
| Task identity and package digest | Bind the occurrence to the target logical task and preserve the digest if present. | The source did not identify a Git revision or sandbox image. ARIES logs these fields as unverified, not different. |
| Verifier result | Reuse the recorded reward of `1.0`. Preserve a score only if the source provides it in `verifier_result.rewards`. | Four tests passed, but the converter does not infer a score from those tests. No verifier or judge runs in replay. |

For comparable captures, an unequal byte count or an unequal exit code fails
replay. Equal byte counts with different comparable content are diagnostic.
If both replay streams have data, a matching sum does not establish byte
equality because the source stream order is unknown. Known truncation makes
output comparison inconclusive. The sample requested an output limit of 12,000
tokens. No truncation marker was observed, but that does not prove complete
capture. The caller can report a strict failure even when the audit has no
corresponding record. See [replay results](../run-results.md#sandbox-call-replay-results).

The inspected artifacts were `agent/codex.txt`, a session rollout under
`agent/sessions/`, `agent/trajectory.json`, `config.json`, `lock.json`,
`result.json`, and verifier output. The source data is not required to run the
synthetic CLI integration test.
