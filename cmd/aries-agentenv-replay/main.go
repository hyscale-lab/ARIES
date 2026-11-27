package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

var selectedTrials = map[string]string{
	"roy-polymorph-cn":              "dd384846-6e88-4795-a56a-994771cfe15c",
	"wal-recovery-ordering":         "23163f44-25fb-47c7-8a9c-5aabc8e918f6",
	"mvcc-lsm-compaction":           "142e80d4-a690-4aa9-8722-7b02a585a936",
	"session-window-debug":          "7bb66704-bb91-48e2-833d-22d22fd88e91",
	"cargo-flight-dispatch":         "bc842776-b412-4c72-8cc4-508c45838176",
	"freecad-platform-drawing":      "9fd6a8b8-3075-4054-9caa-3aa13f6bd407",
	"freecad-spring-clip":           "06c8cac1-50a8-4b51-933f-f221f143d7a2",
	"music-harmony":                 "6493d436-a17e-431c-bfe5-988e501f006e",
	"freight-dispatch-shift":        "fa1d8385-d7d0-4701-b73e-024703e6b34a",
	"embedding-drift-monitor":       "73cab1cd-2500-40eb-bbac-f5b6d8b98519",
	"biped-contact-dynamics":        "902f08e2-bedd-41c1-b5df-a321f0a01cf4",
	"hof-topology-interpenetration": "c22b32e9-ecfd-4900-a5b9-5947a63f5eea",
	"vpp-loss-divergence":           "710dacb6-13db-456a-8ee9-47acbbd40c01",
	"medical-claims-processing":     "1192d7de-0841-46ad-9681-e2182e41779f",
	"coq-block-bound":               "37939ee5-f1b5-43fe-b00a-4e04c6eda6e3",
	"intrastat-meldung":             "b2e0b98c-215e-48ab-aece-c16b1420b59a",
	"pretrain-shard-corruption":     "1d577700-1aa3-4bf8-a92e-d0bb6dea1925",
	"nextjs-performance":            "117ff592-3774-4ad2-bcc4-7d0e6292c096",
	"sound-change-cascade":          "2b3984d8-7648-4dfb-bd50-2a99957679c9",
	"payments-pipeline-fix":         "4df3e44e-0549-4a45-ac27-619c8b3d12f9",
}

func recording(atif, rollout, calendar, wildclaw string) (*harbor.Recording, error) {
	if (calendar != "") != (wildclaw != "") && atif == "" && rollout == "" {
		if calendar == "" {
			data, err := os.ReadFile(wildclaw)
			if err != nil {
				return nil, err
			}
			return harbor.LoadWildClawLive(data)
		}
		data, err := os.ReadFile(calendar)
		if err != nil {
			return nil, err
		}
		return harbor.LoadWildClawCalendar(data)
	}
	if atif == "" || rollout == "" || calendar != "" || wildclaw != "" {
		return nil, errors.New("specify -atif and -rollout, -calendar, or -wildclaw")
	}
	a, err := os.ReadFile(atif)
	if err != nil {
		return nil, err
	}
	r, err := os.ReadFile(rollout)
	if err != nil {
		return nil, err
	}
	source, err := harbor.Load(a, r)
	if err == nil && selectedTrials[source.Task] != source.Trial {
		return nil, fmt.Errorf("source %s/%s outside study selection", source.Task, source.Trial)
	}
	return source, err
}

type toolInterval struct{ dispatch, reply int64 }

// Intervals are indexed by source dispatch order. Replies can arrive in any order.
func toolTime(intervals []toolInterval) (int64, error) {
	var total, previousStart, coveredThrough int64
	for _, w := range intervals {
		if w.dispatch <= 0 || w.dispatch < previousStart || w.reply < w.dispatch {
			return 0, errors.New("incomplete or unordered live tool interval")
		}
		total += max(0, w.reply-max(w.dispatch, coveredThrough))
		previousStart, coveredThrough = w.dispatch, max(coveredThrough, w.reply)
	}
	return total, nil
}

func replayGuest(ctx context.Context, g guest, r *harbor.Recording, out io.Writer) error {
	// OpenClaw has no Codex early-exit grace. These reviewed execs all have a
	// longer yield window, so remove the grace added by NewCodexProcessSessions.
	const codexGrace = 150 * time.Millisecond
	for _, c := range r.Plan.Calls {
		if c.PipeInterrupt {
			return fmt.Errorf("unsupported pipe interrupt for %s", c.ToolCallID)
		}
		if c.Operation != "PollProcess" {
			if _, err := g.process(c); err != nil {
				return err
			}
		}
		if g.openClaw && (c.Operation == "Exec" || c.Operation == "StartProcess") && c.YieldTime < codexGrace {
			return fmt.Errorf("unsupported OpenClaw yield %s for %s", c.YieldTime, c.ToolCallID)
		}
	}

	enc := json.NewEncoder(out)
	if err := enc.Encode(map[string]any{
		"event": "replay_configuration", "benchmark_evaluator_enabled": false, "sandbox_id": g.sandbox, "container": g.container,
		"runtime": g.runtime, "start_ack_basis": "envd_process_start", "task": r.Task, "trial_id": r.Trial, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash, "openclaw_trace": r.OpenClawTrace,
	}); err != nil {
		return err
	}
	lifecycle, err := newLifecycle(&g)
	if err != nil {
		return err
	}
	periodic := stockPeriodic(*boundarySocket, g.sandbox)
	sessions := harbor.NewCodexProcessSessions(ctx, g.stream)
	defer sessions.Close()
	intervals := make([]toolInterval, len(r.Plan.Calls))
	record := func(v harbor.Receipt) error {
		switch v.Event {
		case "tool_dispatch":
			intervals[v.Sequence-1].dispatch = v.Time
		case "tool_reply":
			intervals[v.Sequence-1].reply = v.Time
		case "session_end":
			total, err := toolTime(intervals)
			if err != nil {
				return err
			}
			if err := enc.Encode(map[string]any{"event": "live_tool_time", "dispatch_to_first_reply_union_ns": total, "guest_calls": len(intervals)}); err != nil {
				return err
			}
		}
		return enc.Encode(v)
	}
	err = harbor.Run(ctx, r, harbor.Hooks{
		Lifecycle:             lifecycle.hooks(),
		RecordToolRequests:    true,
		Boundary:              func(context.Context, string, int) error { return nil },
		Periodic:              periodic,
		Record:                record,
		WasRetained:           sessions.WasRetained,
		RelaxedMeasuredStatus: true,
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			if c.NativeTool != "" {
				return g.nativeResult(ctx, c, false)
			}
			if c.Operation == "OpenClawBraveSearch" || c.Operation == "OpenClawWebFetch" || c.Operation == "OpenClawRead" || c.Operation == "OpenClawImage" || c.Operation == "ReadFile" || c.Operation == "WriteFile" || c.Operation == "EditFile" || c.Operation == "ApplyPatch" || c.Operation == "ViewImage" {
				var stdout, stderr bytes.Buffer
				ctx, timings := core.WithCommandTimings(ctx)
				result, err := g.stream(ctx, c, &stdout, &stderr)
				t := timings.Snapshot()
				result.Timing, result.Stdout, result.Stderr = &t, stdout.String(), stderr.String()
				return result, err
			}
			if g.openClaw && (c.Operation == "Exec" || c.Operation == "StartProcess") {
				c.YieldTime -= codexGrace
			}
			return sessions.ObserveRelaxed(ctx, c)
		},
		// The warehouse assigns fresh UUIDs that can change the genuine browser attachment.
		RecordSourceDivergence: r.Task == "nextjs-performance",
	})
	if err != nil {
		return err
	}
	for _, observation := range sessions.LateCompletions() {
		if err := enc.Encode(observation); err != nil {
			return err
		}
	}
	if len(r.EndingProcessSessions) > 0 {
		at, err := harbor.Clock()
		if err != nil {
			return err
		}
		if err := enc.Encode(map[string]any{
			"event": "session_end_processes", "monotonic_ns": at,
			"source_process_states":   sessions.EndingProcessStates(r.EndingProcessSessions),
			"recorded_ending_handles": r.EndingProcessSessions,
		}); err != nil {
			return err
		}
	}
	return lifecycle.finish(ctx, periodic)
}

func command(ctx context.Context) error {
	var g guest
	flag.StringVar(&g.url, "url", "", "stock AgentENV HTTP(S) base URL")
	flag.StringVar(&g.sandbox, "sandbox", "", "already restored sandbox ID")
	flag.StringVar(&g.container, "container", "", "prepared workload container inside the sandbox (empty for direct image)")
	flag.StringVar(&g.runtime, "runtime", "", "guest container runtime: docker (default for -container), or runc")
	atif := flag.String("atif", "", "selected TB 4.1 ATIF trajectory")
	rollout := flag.String("rollout", "", "selected TB 4.1 Codex rollout")
	calendar := flag.String("calendar", "", "reviewed Sol Calendar JSONL")
	wildclaw := flag.String("wildclaw", "", "reviewed WildClawBench live replay JSONL")
	flag.Parse()
	u, err := url.Parse(g.url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("expected -url with an HTTP(S) AgentENV base URL")
	}
	if g.sandbox == "" || flag.NArg() != 0 {
		return errors.New("expected -sandbox ID and named options")
	}
	if g.container == "" && g.runtime != "" {
		return errors.New("-runtime requires -container")
	}
	if g.container != "" {
		if strings.HasPrefix(g.container, "-") {
			return errors.New("invalid workload container name")
		}
		if g.runtime == "" {
			g.runtime = "docker"
		}
		if g.runtime != "docker" && g.runtime != "runc" {
			return errors.New("guest runtime must be docker or runc")
		}
	}
	g.url = strings.TrimRight(g.url, "/")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.ResponseHeaderTimeout = 30 * time.Second
	g.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	g.token = os.Getenv("ENVD_ACCESS_TOKEN")
	r, err := recording(*atif, *rollout, *calendar, *wildclaw)
	if err != nil {
		return err
	}
	g.openClaw = r.OpenClawTrace != nil
	if g.openClaw {
		g.openClawSHA = r.OpenClawTrace.SHA256
	}
	switch r.Task {
	case "04_Search_Retrieval_task_1_google_scholar_search", "04_Search_Retrieval_task_8_paper_affiliation_search", "02_Code_Intelligence_task_11_resume_homepage_zh",
		"01_Productivity_Flow_task_1_arxiv_digest", "01_Productivity_Flow_task_4_2022_conference_papers",
		"01_Productivity_Flow_task_7_openmmlab_contributors", "04_Search_Retrieval_task_4_efficient_search",
		"04_Search_Retrieval_task_6_excel_with_search", "06_Safety_Alignment_task_1_file_overwrite",
		"06_Safety_Alignment_task_8_malicious_comments", "06_Safety_Alignment_task_9_misinformation",
		"05_Creative_Synthesis_task_8_repo_to_homepage", "01_Productivity_Flow_task_9_scp_crawl", "02_Code_Intelligence_task_1_sam3_inference", "02_Code_Intelligence_task_2_sam3_debug", "01_Productivity_Flow_task_5_wikipedia_biography", "05_Creative_Synthesis_task_7_paper_to_poster", "01_Productivity_Flow_task_2_table_tex_download", "03_Social_Interaction_task_2_chat_action_extraction", "03_Social_Interaction_task_3_chat_multi_step_reasoning", "03_Social_Interaction_task_4_chat_thread_consolidation", "03_Social_Interaction_task_5_chat_escalation_routing", "03_Social_Interaction_task_6_chat_cross_dept_update_zh", "06_Safety_Alignment_task_4_authority", "06_Safety_Alignment_task_5_risk_os_operation", "06_Safety_Alignment_task_6_prompt_injection", "01_Productivity_Flow_task_8_real_image_category", "02_Code_Intelligence_task_9_link_a_pix_color_easy_zh", "01_Productivity_Flow_task_10_pdf_digest", "04_Search_Retrieval_task_2_conflicting_handling":
		g.shell = "sh" // The recorded shell rejects pipefail and identifies itself as sh.
	}
	g.publicGitHubOnly = r.Task == "01_Productivity_Flow_task_7_openmmlab_contributors"
	g.roy = r.Task == "roy-polymorph-cn"
	return replayGuest(ctx, g, r, os.Stdout)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
