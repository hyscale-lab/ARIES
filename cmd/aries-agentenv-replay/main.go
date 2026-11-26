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

const royTrial = "dd384846-6e88-4795-a56a-994771cfe15c"

func recording(atif, rollout, calendar string) (*harbor.Recording, error) {
	if calendar != "" && atif == "" && rollout == "" {
		data, err := os.ReadFile(calendar)
		if err != nil {
			return nil, err
		}
		return harbor.LoadWildClawCalendar(data)
	}
	if atif == "" || rollout == "" || calendar != "" {
		return nil, errors.New("specify either -atif and -rollout, or -calendar")
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
	if err == nil && (source.Task != "roy-polymorph-cn" || source.Trial != royTrial) {
		return nil, errors.New("expected the selected Roy recording " + royTrial)
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
		if g.calendar && c.Operation == "Exec" && c.YieldTime < codexGrace {
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
		FinishObservedProcess: sessions.PollObservedProcess,
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			if c.Operation == "ReadFile" {
				var stdout, stderr bytes.Buffer
				ctx, timings := core.WithCommandTimings(ctx)
				result, err := g.stream(ctx, c, &stdout, &stderr)
				t := timings.Snapshot()
				result.Timing, result.Stdout, result.Stderr = &t, stdout.String(), stderr.String()
				return result, err
			}
			if g.calendar && c.Operation == "Exec" {
				c.YieldTime -= codexGrace
			}
			return sessions.ObserveRelaxed(ctx, c)
		},
	})
	if err != nil {
		return err
	}
	return lifecycle.finish(ctx, periodic)
}

func command(ctx context.Context) error {
	var g guest
	flag.StringVar(&g.url, "url", "", "stock AgentENV HTTP(S) base URL")
	flag.StringVar(&g.sandbox, "sandbox", "", "already restored sandbox ID")
	flag.StringVar(&g.container, "container", "", "prepared workload container inside the sandbox (empty for direct image)")
	flag.StringVar(&g.runtime, "runtime", "", "guest container runtime: docker (default for -container), or runc")
	atif := flag.String("atif", "", "selected Roy ATIF trajectory")
	rollout := flag.String("rollout", "", "selected Roy Codex rollout")
	calendar := flag.String("calendar", "", "reviewed Sol Calendar JSONL")
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
	r, err := recording(*atif, *rollout, *calendar)
	if err != nil {
		return err
	}
	g.calendar = r.OpenClawTrace != nil
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
