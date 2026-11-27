package harbor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWildClawSourcePlayback(t *testing.T) {
	// Keep the fixture's source gaps and reply order, with millisecond-scale timestamps.
	trace := strings.NewReplacer("00:01Z", "00:00.001Z", "00:05Z", "00:00.005Z",
		"00:06Z", "00:00.006Z", "00:08Z", "00:00.008Z", "00:10Z", "00:00.010Z",
		"00:12Z", "00:00.012Z", "00:13Z", "00:00.013Z").Replace(wildClawSourceFixture)
	src := []byte(trace)
	if _, err := LoadWildClawLive(src); err == nil {
		t.Fatal("live admission accepted synthetic bytes")
	}
	r, err := LoadWildClawTimeline(src)
	if err != nil {
		t.Fatal(err)
	}
	if !r.SourcePlayback || len(r.Plan.Calls) != 0 || r.OpenClawTrace.Revision != "" {
		t.Fatal("source observations acquired a live plan or published revision")
	}
	body, err := json.Marshal(r)
	if err != nil || strings.Contains(string(body), "printf example") || strings.Contains(string(body), "guest_sequence") {
		t.Fatal("source playback exported a guest command or guest sequence")
	}
	var waits []time.Duration
	var ids, statuses []string
	var sequences []uint64
	var events []string
	if err := Run(t.Context(), r, Hooks{
		Periodic: func(context.Context, string, int, []string) (Freeze, error) { return Freeze{}, nil },
		Record: func(rec Receipt) error {
			events = append(events, rec.Event)
			switch rec.Event {
			case "tool_dispatch", "tool_reply":
				t.Fatalf("source playback reported live work: %v", rec)
			case "session_start", "session_end":
				if !rec.SourcePlayback || rec.SourceTraceStatus != "completed" {
					t.Fatal(rec)
				}
			case "wait_end":
				waits = append(waits, time.Duration(rec.RequestedNS))
				if rec.RunnableActualNS == nil || *rec.RunnableActualNS < *rec.RunnableTargetNS {
					t.Fatal("source clock returned before the deadline")
				}
			case "source_event":
				ids = append(ids, rec.SourceRecordID)
				sequences = append(sequences, rec.Sequence)
				if rec.SourceKind == "toolResult" {
					statuses = append(statuses, rec.SourceReplyStatus)
					if rec.SourceReplyError == nil || *rec.SourceReplyError {
						t.Fatal(rec)
					}
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"user", "request", "request", "image-result", "running-result", "poll", "poll-result", "end"}) ||
		!reflect.DeepEqual(sequences, []uint64{0, 1, 2, 2, 1, 3, 3, 0}) ||
		!reflect.DeepEqual(statuses, []string{"200", "running", "error"}) ||
		!reflect.DeepEqual(waits, []time.Duration{4 * time.Millisecond, time.Millisecond, 2 * time.Millisecond, 2 * time.Millisecond, 2 * time.Millisecond, time.Millisecond}) ||
		events[len(events)-1] != "session_end" {
		t.Fatalf("observed ids %v, sequences %v, statuses %v, waits %v", ids, sequences, statuses, waits)
	}
}

func TestWildClawInterruptedPlayback(t *testing.T) {
	trace, _, _ := strings.Cut(wildClawSourceFixture, `{"type":"message","id":"poll-result"`)
	trace = strings.Replace(trace, `"trace_status":"completed"`, `"trace_status":"interrupted"`, 1)
	r, err := LoadWildClawTimeline([]byte(trace))
	if err != nil {
		t.Fatal(err)
	}
	last := r.Events[len(r.Events)-1]
	if !last.Unanswered || last.CallID != "third" || !reflect.DeepEqual(r.OpenClawTrace.UnansweredCalls, []string{"third"}) ||
		!r.OpenClawTrace.End.IsZero() || !r.OpenClawTrace.ObservedThrough.Equal(last.SourceTime) || r.Plan.FinalWait != 0 {
		t.Fatal("interrupted source lost its request, or acquired an invented end or reply")
	}
}

func TestWildClawSolEngineCoverage(t *testing.T) {
	dir := os.Getenv("WILDCLAW_SOL_TRACE_DIR")
	if dir == "" {
		t.Skip("set WILDCLAW_SOL_TRACE_DIR to the 60 original Sol JSONL files")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(paths) != 60 {
		t.Fatalf("found %d Sol sources: %v", len(paths), err)
	}
	var observedCalls, observedReplies, livePlans, unanswered int
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		r, err := LoadWildClawTimeline(b)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		var total time.Duration
		for _, e := range r.Events {
			total += e.Wait
			if e.Sequence != 0 {
				t.Fatalf("%s observation has guest sequence", p)
			}
			if e.SourceSequence != 0 {
				if e.isToolReply() {
					observedReplies++
				} else {
					observedCalls++
				}
			}
			if e.Unanswered {
				unanswered++
			}
		}
		if total != r.OpenClawTrace.ObservedThrough.Sub(r.Plan.StartedAt) || len(r.Plan.Calls) != 0 {
			t.Fatalf("%s duration %v, %d guest calls", p, total, len(r.Plan.Calls))
		}
		live, err := LoadWildClawLive(b)
		if err != nil {
			if reviewedWildClawLive(r.OpenClawTrace.SHA256) {
				t.Fatalf("%s: %v", p, err)
			}
			continue
		}
		livePlans++
		if braveSearchSource(r.OpenClawTrace.SHA256) {
			var operations [2]int
			for _, c := range live.Plan.Calls {
				switch c.Operation {
				case "OpenClawBraveSearch":
					operations[0]++
				case "OpenClawWebFetch":
					operations[1]++
				default:
					continue
				}
				if c.ToolCallID == "" || len(c.Body) == 0 || len(c.ResponseStdout) == 0 || (c.ExitCode != 0 && (c.Operation != "OpenClawWebFetch" || c.ResponseCode != "error")) {
					t.Fatalf("unbound web operation in %s, sequence %d", r.Task, c.Sequence)
				}
			}
			expected := map[string][2]int{
				"04_Search_Retrieval_task_1_google_scholar_search":    {20, 5},
				"04_Search_Retrieval_task_8_paper_affiliation_search": {8, 0},
				"02_Code_Intelligence_task_11_resume_homepage_zh":     {1, 5},
				"01_Productivity_Flow_task_1_arxiv_digest":            {3, 0},
				"01_Productivity_Flow_task_4_2022_conference_papers":  {20, 7},
				"01_Productivity_Flow_task_7_openmmlab_contributors":  {1, 0},
				"04_Search_Retrieval_task_4_efficient_search":         {4, 6},
				"06_Safety_Alignment_task_1_file_overwrite":           {1, 0},
				"06_Safety_Alignment_task_8_malicious_comments":       {1, 2},
				"06_Safety_Alignment_task_9_misinformation":           {3, 4},
				"04_Search_Retrieval_task_6_excel_with_search":        {3, 0},
				"04_Search_Retrieval_task_10_tomllib_trace":           {3, 3},
				"04_Search_Retrieval_task_5_fuzzy_search":             {3, 4},
				"04_Search_Retrieval_task_11_fuzzy_repo_search":       {3, 9},
				"04_Search_Retrieval_task_3_constraint_search":        {7, 2},
			}
			if operations != expected[r.Task] {
				t.Fatalf("%s: projected search/fetch counts %v", r.Task, operations)
			}
		}
		if r.OpenClawTrace.SHA256 == wildClawImageCategoryDigest {
			var exit []int32
			for _, c := range live.Plan.Calls {
				if c.Workdir != "/root" || c.Operation != "Exec" || c.Script == "" || c.YieldTime != 10*time.Second {
					t.Fatalf("image classification changed shell call %d", c.Sequence)
				}
				exit = append(exit, c.ExitCode)
			}
			if !reflect.DeepEqual(exit, []int32{2, 0, 1, 0, 0, 0}) {
				t.Fatalf("image classification expected exits %v", exit)
			}
		}
	}
	if observedCalls != 1109 || observedReplies != 1108 || unanswered != 1 || livePlans != 36 {
		t.Fatalf("observed %d requests, %d replies, %d unanswered, %d live plans", observedCalls, observedReplies, unanswered, livePlans)
	}
}
