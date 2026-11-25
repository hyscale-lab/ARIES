# Synthetic Harbor replay source

This example uses synthetic data. `TestReplayHarborConvertedExample` loads its
files and checks both requests against a local gRPC endpoint.

## Files and source identity

- `run-result.json` contains `run_id` and `tasks`. Each `tasks` entry uses `task_id`
  for an occurrence ID, not a logical task ID. Include `evaluation` only if the
  source supplies a grade. This example has no grade.
- `task-001/bridge/runtime.json` identifies the occurrence. Set `schema_version`
  to `1`, `replay_ready` to `true`, and `origin_type` to `"harbor"`. Set `run_id`
  to the parent run ID and `occurrence_id` to the directory name. Set `task_id`
  to the logical task ID and `benchmark_type` to the target benchmark type.
  Set `SandboxID` to a nonempty source identifier and use that same value in
  each call. Keep the capitalization of `SandboxID`.
- Set `harness_completed_at` to the agent phase end time. Set `tool_calls_bytes`
  and `tool_calls_sha256` to the byte count and lowercase hex SHA-256 of the
  finalized `tool-calls.jsonl`, including the last newline. Omit unknown `image`,
  `benchmark_revisions`, and `task_package_digest` values. Do not infer them.
- Set the source root, occurrence, and bridge directories to mode `0700`.
  Set the three files to `0600`. Git does not store these mode bits.

## Call records

Write one UTF-8 JSON object per line and end each line with a newline. Use
consecutive `sequence` values starting at `1`. Use recorded `started_at`
(request) and `timestamp` (return) in UTC, with RFC3339 nanosecond precision
when available. `run_id`, `task_id`, and `sandbox_id` bind every record to
the runtime metadata. Here `task_id` is the occurrence ID. `tool_call_id` is
optional. `duration_ms` is optional; if present, it must be nonnegative.
Use the exact field names, without duplicates. Use valid Unicode strings;
unpaired surrogate escapes are not accepted.

- **Exec:** Set `operation_class` to `"exec"`. Keep the recorded shell `command`,
  its lowercase hex SHA-256 as `command_hash`, `path` as `"/bin/bash"`, and the
  recorded `workdir`. Set `argv` to the exact shell arguments, including the
  executable path and `"-l"` if applicable. Set `stdin` to `""`, `stdin_encoding`
  to `"utf-8"`, and `stdin_bytes` to `0`. Use `status: "completed"` for a returned
  command and supply its integer `exit_code`, including explicit zero.
  Set `harbor_output` to the decoded combined output string, including an
  explicit `""` for no output. Keep whitespace and final newlines. Do not
  include status or timing headers. Set `harbor_truncated` to `true` if the
  source capture is known to be cut. Omit it or use `false` if truncation is
  not known. This does not prove the process output was complete. If either
  side reports truncation, the output comparison is inconclusive: the source
  has no evidence that the two capture limits are equivalent.
- **Successful add-file:** Set `operation_class` to `"write"`, `harbor_patch`
  to `"add"`, and `status` to `"completed"`. Set `path` to the absolute file
  path and `stdin` to the complete new content. Set `stdin_encoding` to
  `"utf-8"`, `stdin_bytes` to the UTF-8 byte count, and `sha256` to the
  lowercase hex SHA-256 of those bytes. Do not add `harbor_output` or
  `harbor_truncated` to a write record.

Do not emit native `response_sha256`, `response_bytes`, `grpc_code`, response
debug fields, or `truncated` in a converted call. The private staged caller
plan carries `harbor_output` and `harbor_truncated` only for `Exec`. It carries
the validated add-file request as `WriteFile`, not as a patch or shell command.
