// Package harbor binds Codex ATIF steps to native rollout events for guest replay.
package harbor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

const (
	Job = "17d1a7f6-3339-4670-8b70-3b145979f57f"
	Row = "f8d623c8-5f1b-4f9c-8deb-d110fa91e1d2"
)

//go:embed reviewed.json
var reviewed []byte

type toolCall struct {
	ID   string          `json:"tool_call_id"`
	Name string          `json:"function_name"`
	Args json.RawMessage `json:"arguments"`
}
type Step struct {
	ID        int       `json:"step_id"`
	Source    string    `json:"source"`
	Timestamp time.Time `json:"timestamp"`
}
type sourceStep struct {
	Step
	Tools       []toolCall `json:"tool_calls"`
	Observation struct {
		Results []struct {
			ID      string `json:"source_call_id"`
			Content string `json:"content"`
		} `json:"results"`
	} `json:"observation"`
}

// Event points into the retained rollout, which preserves original message,
// reasoning, plan update, and tool payloads without copying them to run logs.
type Event struct {
	Line       int           `json:"rollout_line"`
	SourceTime time.Time     `json:"source_time"`
	Kind       string        `json:"kind"`
	Role       string        `json:"role,omitempty"`
	Step       int           `json:"step_id,omitempty"`
	CallID     string        `json:"call_id,omitempty"`
	Sequence   uint64        `json:"guest_sequence,omitempty"`
	Wait       time.Duration `json:"modeled_wait_ns"`
	Basis      string        `json:"wait_basis"`
}

type workload struct {
	Task    string   `json:"task"`
	Workdir string   `json:"workdir"`
	Image   string   `json:"image"`
	Effects []string `json:"effects"`
	Trials  []struct {
		Trial      string
		Trajectory string `json:"trajectory_sha256"`
		Rollout    string `json:"rollout_sha256"`
	} `json:"trials"`
}

type Recording struct {
	Task            string      `json:"task"`
	Workdir         string      `json:"workdir"`
	Image           string      `json:"image"`
	ReviewedEffects []string    `json:"reviewed_effects"`
	Trial           string      `json:"trial_id"`
	Job             string      `json:"job_id"`
	Row             string      `json:"row_id"`
	Session         string      `json:"session_id"`
	Model           string      `json:"model"`
	TrajectoryHash  string      `json:"trajectory_sha256"`
	RolloutHash     string      `json:"rollout_sha256"`
	Steps           []Step      `json:"steps"`
	Events          []Event     `json:"events"`
	Plan            replay.Plan `json:"-"`
}

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// Load accepts the reviewed benchmark occurrence. A separate digest binds each
// artifact because the native call emission can be later than the ATIF step.
func Load(atif, rollout []byte) (*Recording, error) {
	var entries []workload
	if err := json.Unmarshal(reviewed, &entries); err != nil {
		return nil, err
	}
	ta, ro := digest(atif), digest(rollout)
	for _, w := range entries {
		for _, entry := range w.Trials {
			if entry.Trajectory == ta && entry.Rollout == ro {
				return parse(w, entry.Trial, atif, rollout)
			}
		}
	}
	return nil, errors.New("source bytes do not match a reviewed Harbor trial")
}

var exitHeader = regexp.MustCompile(`(?m)^Process exited with code ([0-9]+)\r?$`)

type indexedCall struct {
	step   int
	call   toolCall
	output string
}

func parse(w workload, trial string, atif, rollout []byte) (*Recording, error) {
	var t struct {
		Schema  string `json:"schema_version"`
		Session string `json:"session_id"`
		Agent   struct {
			Name  string
			Model string `json:"model_name"`
		} `json:"agent"`
		Steps []sourceStep `json:"steps"`
	}
	if err := json.Unmarshal(atif, &t); err != nil {
		return nil, err
	}
	if t.Schema != "ATIF-v1.7" || t.Session == "" || t.Agent.Name != "codex" || t.Agent.Model != "gpt-6-astra" {
		return nil, errors.New("unexpected ATIF identity")
	}
	r := &Recording{Task: w.Task, Workdir: w.Workdir, Image: w.Image, ReviewedEffects: w.Effects, Trial: trial, Job: Job, Row: Row, Session: t.Session, Model: t.Agent.Model, TrajectoryHash: digest(atif), RolloutHash: digest(rollout)}
	r.Plan.SourceRunID = Job
	r.Plan.SourceTaskID = trial
	calls := map[string]indexedCall{}
	for i, s := range t.Steps {
		if s.ID != i+1 || s.Timestamp.IsZero() || len(s.Tools) > 1 || len(s.Tools) != len(s.Observation.Results) {
			return nil, fmt.Errorf("unsupported ATIF step %d", s.ID)
		}
		r.Steps = append(r.Steps, s.Step)
		for _, c := range s.Tools {
			result := s.Observation.Results[0]
			if c.ID == "" || calls[c.ID].step != 0 || c.ID != result.ID || (c.Name != "exec_command" && c.Name != "update_plan") {
				return nil, fmt.Errorf("unsupported or ambiguous call at step %d", s.ID)
			}
			calls[c.ID] = indexedCall{s.ID, c, result.Content}
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(rollout))
	sc.Buffer(make([]byte, 65536), replay.MaxPlanBytes)
	var previous time.Time
	var active, activeTool string
	var lastStep int
	var cwd string
	var complete, sessionBound bool
	started := map[string]bool{}
	for line := 1; sc.Scan(); line++ {
		var raw struct {
			Time    time.Time `json:"timestamp"`
			Type    string    `json:"type"`
			Payload struct {
				Type, Role, Name, Arguments, Output string
				CallID                              string `json:"call_id"`
				Cwd                                 string `json:"cwd"`
				SessionID                           string `json:"session_id"`
			}
		}
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			return nil, fmt.Errorf("rollout line %d: %w", line, err)
		}
		p := raw.Payload
		e := Event{Line: line, SourceTime: raw.Time, Kind: p.Type, Role: p.Role, CallID: p.CallID, Basis: "inferred_inter_event_gap"}
		if raw.Type == "turn_context" {
			if p.Cwd != w.Workdir {
				return nil, fmt.Errorf("source working directory %q differs from reviewed %q", p.Cwd, w.Workdir)
			}
			cwd = p.Cwd
			continue
		}
		if raw.Type == "session_meta" {
			sessionBound = p.SessionID == t.Session
			continue
		}
		switch {
		case raw.Type == "event_msg" && p.Type == "task_started":
			if !previous.IsZero() {
				return nil, errors.New("multiple turns unsupported")
			}
			r.Plan.StartedAt = raw.Time
			e.Basis = "source_origin"
		case raw.Type == "event_msg" && p.Type == "task_complete":
			complete = true
		case raw.Type == "response_item":
			if previous.IsZero() || complete {
				return nil, errors.New("response outside source turn")
			}
		default:
			continue
		}
		if raw.Time.IsZero() || (!previous.IsZero() && raw.Time.Before(previous)) {
			return nil, fmt.Errorf("invalid source time at line %d", line)
		}
		if !previous.IsZero() {
			e.Wait = raw.Time.Sub(previous)
		}
		previous = raw.Time
		switch p.Type {
		case "function_call":
			c, ok := calls[p.CallID]
			if !ok || started[p.CallID] || p.Name != c.call.Name || active != "" || !equalJSON([]byte(p.Arguments), c.call.Args) {
				return nil, fmt.Errorf("call binding or concurrency mismatch at line %d", line)
			}
			active, activeTool = p.CallID, p.Name
			lastStep = c.step
			started[active] = true
			e.Step = c.step
			e.Kind = p.Name
			if p.Name == "exec_command" {
				call, err := guestCall(c, cwd, r.Plan.StartedAt, raw.Time, uint64(len(r.Plan.Calls)+1))
				if err != nil {
					return nil, err
				}
				r.Plan.Calls = append(r.Plan.Calls, call)
				e.Sequence = call.Sequence
			}
		case "function_call_output":
			if active == "" || p.CallID != active || p.Output != calls[active].output {
				return nil, fmt.Errorf("reply binding mismatch at line %d", line)
			}
			e.Step = lastStep
			if activeTool == "exec_command" {
				e.Basis = "replaced_by_live_guest_reply"
				e.Wait = 0
				e.Sequence = uint64(len(r.Plan.Calls))
			} else {
				e.Basis = "recorded_harness_tool_interval"
			}
			active, activeTool = "", ""
		case "message", "reasoning", "task_started", "task_complete":
			if active != "" {
				return nil, errors.New("overlapping response items unsupported")
			}
		default:
			return nil, fmt.Errorf("unsupported native item %q at line %d", p.Type, line)
		}
		r.Events = append(r.Events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sessionBound || !complete || r.Plan.StartedAt.IsZero() || active != "" || len(started) != len(calls) || len(r.Plan.Calls) == 0 {
		return nil, errors.New("incomplete or unbound source occurrence")
	}
	return r, nil
}

func equalJSON(a, b []byte) bool {
	var left, right map[string]any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func guestCall(c indexedCall, defaultDir string, origin, ts time.Time, sequence uint64) (replay.Call, error) {
	var args struct {
		Cmd          string `json:"cmd"`
		Workdir      string `json:"workdir"`
		OutputTokens int    `json:"max_output_tokens"`
		YieldMs      int    `json:"yield_time_ms"`
	}
	dec := json.NewDecoder(bytes.NewReader(c.call.Args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return replay.Call{}, err
	}
	if args.Workdir == "" {
		args.Workdir = defaultDir
	}
	header, body, found := strings.Cut(c.output, "\nOutput:\n")
	match := exitHeader.FindStringSubmatch(header)
	if !found || len(match) != 2 || args.Cmd == "" || !path.IsAbs(args.Workdir) {
		return replay.Call{}, fmt.Errorf("step %d has no completed foreground exec or workdir", c.step)
	}
	exit, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil {
		return replay.Call{}, err
	}
	return replay.Call{Operation: "Exec", Sequence: sequence, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, Script: args.Cmd, Workdir: args.Workdir, Login: true, ExitCode: int32(exit), ResponseStdout: []byte(body)}, nil
}
