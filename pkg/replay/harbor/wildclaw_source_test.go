package harbor

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const wildClawSourceFixture = `{"type":"session","version":3,"id":"source","timestamp":"2026-01-02T00:00:00Z","harness":"openclaw","cwd":"/workspace","model":"example","run_id":"run","task_id":"task","score":1,"trace_status":"completed"}
{"type":"model_change","id":"model","parentId":null,"timestamp":"2026-01-02T00:00:00Z"}
{"type":"message","id":"user","parentId":"model","timestamp":"2026-01-02T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Check the document."}]}}
{"type":"message","id":"request","parentId":"user","timestamp":"2026-01-02T00:00:05Z","message":{"role":"assistant","timestamp":1767312004999,"stopReason":"toolUse","content":[{"type":"thinking","thinking":"Choose tools"},{"type":"toolCall","id":"first","name":"exec","arguments":{"command":"printf example","yieldMs": 100}},{"type":"text","text":"Collecting source data."},{"type":"toolCall","id":"second","name":"read","arguments":{"path":"cover.png"}}]}}
{"type":"message","id":"image-result","parentId":"request","timestamp":"2026-01-02T00:00:06Z","message":{"role":"toolResult","toolCallId":"second","toolName":"read","isError":false,"content":[{"type":"text","text":"Cover image"},{"type":"image","data":"AA==","mimeType":"image/png"}],"details":{"status":200}}}
{"type":"message","id":"running-result","parentId":"image-result","timestamp":"2026-01-02T00:00:08Z","message":{"role":"toolResult","toolCallId":"first","toolName":"exec","isError":false,"content":[{"type":"text","text":"Process still running"}],"details":{"status":"running","sessionId":"job-1"}}}
{"type":"message","id":"poll","parentId":"running-result","timestamp":"2026-01-02T00:00:10Z","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"third","name":"process","arguments":{"action":"poll","sessionId":"job-1"}}]}}
{"type":"message","id":"poll-result","parentId":"poll","timestamp":"2026-01-02T00:00:12Z","message":{"role":"toolResult","toolCallId":"third","toolName":"process","isError":false,"content":[{"type":"text","text":"An error occurred"}],"details":{"status":"error","error":"worker lost"}}}
{"type":"message","id":"end","parentId":"poll-result","timestamp":"2026-01-02T00:00:13Z","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"Done."}]}}
`

func TestParseWildClawSource(t *testing.T) {
	source, err := ParseWildClawSource([]byte(wildClawSourceFixture))
	if err != nil {
		t.Fatal(err)
	}
	if source.Version != 3 || source.SessionID != "source" || source.RunID != "run" ||
		source.TaskID != "task" || source.Status != "completed" || source.Score != 1 ||
		len(source.Records) != 9 || len(source.Batches) != 2 || source.End.Sub(source.Start) != 12*time.Second {
		t.Fatal("source header, row count or timeline changed")
	}
	batch := source.Batches[0]
	if batch.Request.Line != 4 || batch.Request.ID != "request" || batch.Request.ParentID != "user" ||
		batch.Request.Time.Sub(source.Start) != 4*time.Second ||
		batch.Request.MessageTimeMS == nil || *batch.Request.MessageTimeMS != 1767312004999 ||
		len(batch.Calls) != 2 {
		t.Fatal("assistant request provenance or batch size changed")
	}
	first, second := batch.Calls[0], batch.Calls[1]
	if first.ID != "first" || first.Sequence != 1 || first.ContentIndex != 1 ||
		second.ID != "second" || second.Sequence != 2 || second.ContentIndex != 3 ||
		string(first.Arguments) != `{"command":"printf example","yieldMs": 100}` {
		t.Fatal("request order, content indices or raw arguments changed")
	}
	if first.Reply == nil || second.Reply == nil ||
		first.Reply.Record.Line != 6 || second.Reply.Record.Line != 5 ||
		first.Reply.Record.ID != "running-result" || first.Reply.Record.ParentID != "image-result" ||
		first.Reply.Record.Time.Sub(batch.Request.Time) != 3*time.Second ||
		first.Reply.Status != "running" || second.Reply.Status != "200" ||
		string(second.Reply.Details) != `{"status":200}` ||
		string(second.Reply.Content) != `[{"type":"text","text":"Cover image"},{"type":"image","data":"AA==","mimeType":"image/png"}]` {
		t.Fatal("reply ordering, timestamps, raw media or typed statuses changed")
	}
	poll := source.Batches[1].Calls[0]
	if poll.Sequence != 3 || poll.Reply == nil || poll.Reply.IsError == nil || *poll.Reply.IsError ||
		poll.Reply.Status != "error" || string(poll.Arguments) != `{"action":"poll","sessionId":"job-1"}` {
		t.Fatal("asynchronous handle or recorded tool error changed")
	}
}

func TestParseWildClawSourceInterrupted(t *testing.T) {
	trace := strings.Replace(wildClawSourceFixture, `"trace_status":"completed"`,
		`"trace_status":"interrupted","trace_status_reason":"example interruption"`, 1)
	lines := strings.Split(trace, "\n")
	lines[7] = strings.Replace(lines[8], `"parentId":"poll-result"`, `"parentId":"poll"`, 1)
	trace = strings.Join(lines[:8], "\n")
	source, err := ParseWildClawSource([]byte(trace))
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != "interrupted" || source.End.IsZero() || source.StatusReason != "example interruption" ||
		source.Batches[1].Calls[0].Reply != nil {
		t.Fatal("interrupted stop or unanswered tool request changed")
	}
	trace = strings.Join(lines[:7], "\n")
	source, err = ParseWildClawSource([]byte(trace))
	if err != nil || !source.End.IsZero() || source.Batches[1].Calls[0].Reply != nil {
		t.Fatal("interrupted EOF should preserve the unanswered request and leave final time unknown")
	}
}

func TestParseWildClawSourceRejectsInvalidTimeline(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"duplicate record", `"id":"request"`, `"id":"model"`},
		{"nonlinear parent", `"parentId":"user"`, `"parentId":"model"`},
		{"backwards time", `"2026-01-02T00:00:05Z"`, `"2026-01-01T00:00:05Z"`},
		{"missing time", `"timestamp":"2026-01-02T00:00:05Z"`, `"timestamp":null`},
		{"missing request identity", `"id":"first"`, `"id":""`},
		{"duplicate request", `"id":"second"`, `"id":"first"`},
		{"duplicate request across batches", `"id":"third"`, `"id":"first"`},
		{"orphan reply", `"toolCallId":"second"`, `"toolCallId":"unknown"`},
		{"wrong reply tool", `"toolName":"read"`, `"toolName":"write"`},
		{"duplicate reply", `"toolCallId":"third","toolName":"process"`, `"toolCallId":"first","toolName":"exec"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := strings.Replace(wildClawSourceFixture, tc.old, tc.replacement, 1)
			if _, err := ParseWildClawSource([]byte(trace)); err == nil {
				t.Fatal("accepted an invalid source timeline")
			}
		})
	}
	lines := strings.Split(wildClawSourceFixture, "\n")
	lines[7] = strings.Replace(lines[8], `"parentId":"poll-result"`, `"parentId":"poll"`, 1)
	if _, err := ParseWildClawSource([]byte(strings.Join(lines[:8], "\n"))); err == nil {
		t.Fatal("accepted completed session with an unanswered request")
	}
	extra := `{"type":"custom","id":"late","parentId":"end","timestamp":"2026-01-02T00:00:14Z"}`
	if _, err := ParseWildClawSource([]byte(wildClawSourceFixture + extra)); err == nil {
		t.Fatal("accepted a row after final stop")
	}
}

func TestParseWildClawSolCohort(t *testing.T) {
	traceDir := os.Getenv("WILDCLAW_SOL_TRACE_DIR")
	if traceDir == "" {
		t.Skip("set WILDCLAW_SOL_TRACE_DIR to the 60 original publisher JSONL files")
	}
	inventoryPath := os.Getenv("WILDCLAW_SOL_INVENTORY")
	if inventoryPath == "" {
		inventoryPath = "testdata/wildclaw-sol-inventory.csv"
	}
	data, err := os.ReadFile(inventoryPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 61 {
		t.Fatalf("expected 60 cohort inventory entries, found %d", len(rows)-1)
	}
	columns := make(map[string]int)
	for i, name := range rows[0] {
		columns[name] = i
	}
	for _, row := range rows[1:] {
		value := func(column string) string { return row[columns[column]] }
		t.Run(value("task"), func(t *testing.T) {
			trace, err := os.ReadFile(filepath.Join(traceDir, value("task")+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			source, err := ParseWildClawSource(trace)
			if err != nil {
				t.Fatal(err)
			}
			if source.TaskID != value("task") || source.SHA256 != value("sha256") ||
				source.Status != value("source_status") || source.Model != "GPT-5.6 Sol" {
				t.Fatal("source identity differs from the cohort inventory")
			}
			calls, replies, parallel := 0, 0, 0
			var wall time.Duration
			toolCounts := make(map[string]int)
			for _, batch := range source.Batches {
				if len(batch.Calls) > 1 {
					parallel++
				}
				var latest time.Time
				for _, call := range batch.Calls {
					calls++
					toolCounts[call.Name]++
					if call.Sequence != uint64(calls) {
						t.Fatal("source request sequence has a gap")
					}
					if call.Reply != nil {
						replies++
						if call.Reply.Record.Time.After(latest) {
							latest = call.Reply.Record.Time
						}
					}
				}
				// A new assistant batch starts only after the preceding replies.
				if !latest.IsZero() {
					wall += latest.Sub(batch.Request.Time)
				}
			}
			for column, got := range map[string]int{
				"source_calls": calls, "paired_calls": replies, "unanswered_calls": calls - replies,
				"parallel_batches": parallel,
			} {
				if strconv.Itoa(got) != value(column) {
					t.Errorf("%s = %d, expected %s", column, got, value(column))
				}
			}
			var expectedTools map[string]int
			if err := json.Unmarshal([]byte(value("tool_counts")), &expectedTools); err != nil || !maps.Equal(toolCounts, expectedTools) {
				t.Error("tool counts differ from the cohort inventory")
			}
			expectedWall, err := strconv.ParseFloat(value("observed_source_tool_wall_s"), 64)
			if err != nil || math.Abs(wall.Seconds()-expectedWall) > 0.0005 {
				t.Errorf("observed tool wall time = %s, expected %s seconds", wall, value("observed_source_tool_wall_s"))
			}
			checkWildClawSourcePlayback(t, trace, source)
		})
	}
}

func checkWildClawSourcePlayback(t *testing.T, trace []byte, source *WildClawSource) {
	t.Helper()
	recording, err := LoadWildClawTimeline(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !recording.SourcePlayback || len(recording.Plan.Calls) != 0 {
		t.Fatal("source timeline contains an executable plan")
	}
	var totalWait time.Duration
	for i := range recording.Events {
		totalWait += recording.Events[i].Wait
		recording.Events[i].Wait = 0
	}
	span := source.Records[len(source.Records)-1].Time.Sub(source.Start)
	if totalWait != span {
		t.Fatalf("source wait was %s, expected %s from prompt to last record", totalWait, span)
	}

	type observation struct {
		record       WildClawSourceRecord
		kind, callID string
		sequence     uint64
		unanswered   bool
	}
	byLine := make(map[int][]observation)
	for _, batch := range source.Batches {
		for _, call := range batch.Calls {
			line := batch.Request.Line
			byLine[line] = append(byLine[line], observation{batch.Request, call.Name, call.ID, call.Sequence, call.Reply == nil})
			if call.Reply != nil {
				reply := call.Reply.Record
				byLine[reply.Line] = []observation{{reply, "toolResult", call.ID, call.Sequence, false}}
			}
		}
	}
	var expected []observation
	for _, row := range source.Records {
		if len(expected) == 0 && row.Role != "user" {
			continue
		}
		if group := byLine[row.Line]; len(group) > 0 {
			expected = append(expected, group...)
		} else {
			expected = append(expected, observation{record: row, kind: row.Type})
		}
	}

	seen := 0
	var lifecycle []string
	hooks := Hooks{
		Boundary: func(context.Context, string, int) error { return nil },
		Record: func(receipt Receipt) error {
			switch receipt.Event {
			case "session_start", "session_end":
				if !receipt.SourcePlayback || receipt.SourceTraceStatus != source.Status {
					return fmt.Errorf("invalid source session receipt")
				}
				lifecycle = append(lifecycle, receipt.Event)
			case "source_event":
				if seen >= len(expected) {
					return fmt.Errorf("unexpected extra source event")
				}
				want := expected[seen]
				if receipt.Line != want.record.Line || receipt.SourceRecordID != want.record.ID ||
					receipt.SourceParentID != want.record.ParentID || !receipt.SourceTime.Equal(want.record.Time) ||
					receipt.SourceRole != want.record.Role || receipt.SourceKind != want.kind ||
					receipt.SourceCallID != want.callID || receipt.Sequence != want.sequence ||
					receipt.Unanswered != want.unanswered {
					return fmt.Errorf("source event %d differs from recorded line %d", seen+1, want.record.Line)
				}
				seen++
			default:
				return fmt.Errorf("unexpected receipt kind %q in source playback", receipt.Event)
			}
			return nil
		},
	}
	if err := Run(t.Context(), recording, hooks); err != nil {
		t.Fatal(err)
	}
	if seen != len(expected) || len(lifecycle) != 2 || lifecycle[0] != "session_start" || lifecycle[1] != "session_end" {
		t.Fatalf("emitted %d/%d source events and %d/2 lifecycle receipts", seen, len(expected), len(lifecycle))
	}
}
