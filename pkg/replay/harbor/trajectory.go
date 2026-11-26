// Package harbor binds Codex ATIF steps to native rollout events for guest replay.
package harbor

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

const (
	Job                     = "17d1a7f6-3339-4670-8b70-3b145979f57f"
	Row                     = "f8d623c8-5f1b-4f9c-8deb-d110fa91e1d2"
	TerminalBench4SourceJob = "a538f69d-7756-444a-9dfe-43a0ab10b3a8"
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
	ParallelSize int           `json:"parallel_size,omitempty"`
	Line         int           `json:"rollout_line"`
	SourceTime   time.Time     `json:"source_time"`
	Kind         string        `json:"kind"`
	Role         string        `json:"role,omitempty"`
	Step         int           `json:"step_id,omitempty"`
	CallID       string        `json:"call_id,omitempty"`
	Sequence     uint64        `json:"guest_sequence,omitempty"`
	Wait         time.Duration `json:"modeled_wait_ns"`
	Basis        string        `json:"wait_basis"`
}

type workload struct {
	Tools        []string `json:"tools"`
	NeedsNetwork bool     `json:"needs_network"`
	VCPUs        int      `json:"vcpus"`
	MemoryMiB    int      `json:"memory_mib"`
	StorageMiB   int      `json:"storage_mib"`
	Task         string   `json:"task"`
	Workdir      string   `json:"workdir"`
	Image        string   `json:"image"`
	Job          string   `json:"job_id"`
	Row          string   `json:"row_id"`
	Effects      []string `json:"effects"`
	Trials       []struct {
		Trial      string
		Trajectory string `json:"trajectory_sha256"`
		Rollout    string `json:"rollout_sha256"`
	} `json:"trials"`
}

type Recording struct {
	EndingProcessSessions []int       `json:"ending_process_sessions,omitempty"`
	NeedsNetwork          bool        `json:"needs_network"`
	VCPUs                 int         `json:"vcpus"`
	MemoryMiB             int         `json:"memory_mib"`
	StorageMiB            int         `json:"storage_mib"`
	Task                  string      `json:"task"`
	Workdir               string      `json:"workdir"`
	Image                 string      `json:"image"`
	ReviewedEffects       []string    `json:"reviewed_effects"`
	Trial                 string      `json:"trial_id"`
	Job                   string      `json:"job_id"`
	Row                   string      `json:"row_id"`
	Session               string      `json:"session_id"`
	Model                 string      `json:"model"`
	TrajectoryHash        string      `json:"trajectory_sha256"`
	RolloutHash           string      `json:"rollout_sha256"`
	Steps                 []Step      `json:"steps"`
	Events                []Event     `json:"events"`
	Plan                  replay.Plan `json:"-"`
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

var runningHeader = regexp.MustCompile(`(?m)^Process running with session ID ([0-9]+)\r?$`)
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
	r := &Recording{NeedsNetwork: w.NeedsNetwork, VCPUs: w.VCPUs, MemoryMiB: w.MemoryMiB, StorageMiB: w.StorageMiB, Task: w.Task, Workdir: w.Workdir, Image: w.Image, ReviewedEffects: w.Effects, Trial: trial, Job: cmp.Or(w.Job, Job), Row: cmp.Or(w.Row, Row), Session: t.Session, Model: t.Agent.Model, TrajectoryHash: digest(atif), RolloutHash: digest(rollout)}
	r.Plan.SourceRunID = r.Job
	r.Plan.SourceTaskID = trial
	ordinary, web, err := splitWebActions(t.Steps, slices.Contains(w.Tools, "web_search_call"))
	if err != nil {
		return nil, err
	}
	calls := map[string]indexedCall{}
	batchSizes := map[int]int{}
	for i, s := range ordinary {
		if s.ID != i+1 || s.Timestamp.IsZero() || len(s.Tools) > 16 || len(s.Tools) != len(s.Observation.Results) {
			return nil, fmt.Errorf("unsupported ATIF step %d", s.ID)
		}
		r.Steps = append(r.Steps, s.Step)
		if len(s.Tools) > 1 {
			batchSizes[s.ID] = len(s.Tools)
		}
		results := map[string]string{}
		for _, result := range s.Observation.Results {
			if _, exists := results[result.ID]; exists {
				return nil, errors.New("duplicate ATIF reply")
			}
			results[result.ID] = result.Content
		}
		for _, c := range s.Tools {
			output, present := results[c.ID]
			if c.ID == "" || calls[c.ID].step != 0 || !present || (len(s.Tools) > 1 && c.Name == "update_plan") || !slices.Contains(w.Tools, c.Name) {
				return nil, fmt.Errorf("unsupported or ambiguous call at step %d", s.ID)
			}
			calls[c.ID] = indexedCall{s.ID, c, output}
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(rollout))
	sc.Buffer(make([]byte, 65536), replay.MaxPlanBytes)
	var previous time.Time
	active := map[string]uint64{}
	var activeStep int
	var cwd string
	var complete, sessionBound, seenSession bool
	started := map[string]bool{}
	for line := 1; sc.Scan(); line++ {
		var raw struct {
			Time    time.Time `json:"timestamp"`
			Type    string    `json:"type"`
			Payload struct {
				Type, Role, Name, Arguments string
				Output                      json.RawMessage
				CallID                      string `json:"call_id"`
				Cwd                         string `json:"cwd"`
				SessionID                   string `json:"session_id"`
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
			if seenSession {
				return nil, errors.New("duplicate native session metadata")
			}
			seenSession = true
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
			if !ok || started[p.CallID] || p.Name != c.call.Name || (len(active) > 0 && activeStep != c.step) || !equalJSON([]byte(p.Arguments), c.call.Args) {
				return nil, fmt.Errorf("call binding or concurrency mismatch at line %d", line)
			}
			if len(active) > 0 {
				e.Basis = "parallel_dispatch_spacing"
			}
			activeStep = c.step
			started[p.CallID] = true
			e.ParallelSize = batchSizes[c.step]
			e.Step = c.step
			e.Kind = p.Name
			if rejectedBeforeGuest(trial, c) {
				e.Kind = "harness_rejection_request"
			} else if p.Name != "update_plan" {
				call, err := guestCall(c, cwd, r.Plan.StartedAt, raw.Time, uint64(len(r.Plan.Calls)+1))
				if err != nil {
					return nil, err
				}
				r.Plan.Calls = append(r.Plan.Calls, call)
				e.Sequence = call.Sequence
			}
			active[p.CallID] = e.Sequence
		case "function_call_output":
			seq, present := active[p.CallID]
			c := calls[p.CallID]
			if !present || !matchesReply(c, p.Output) {
				return nil, fmt.Errorf("reply binding mismatch at line %d", line)
			}
			e.Step = c.step
			e.ParallelSize = batchSizes[c.step]
			if rejectedBeforeGuest(trial, c) {
				e.Kind = "harness_rejection_reply"
				e.Basis = "recorded_harness_tool_interval"
			} else if c.call.Name != "update_plan" {
				e.Basis = "replaced_by_live_guest_reply"
				e.Wait = 0
				e.Sequence = seq
			} else {
				e.Basis = "recorded_harness_tool_interval"
			}
			delete(active, p.CallID)
		case "web_search_call":
			step, id, err := web.bind(sc.Bytes())
			if err != nil {
				return nil, err
			}
			e.Step = step
			e.CallID = id
			e.Basis = "inferred_inter_event_gap_before_external_completion"
		case "message", "reasoning", "task_started", "task_complete":
			if len(active) > 0 {
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
	if !sessionBound || !complete || !web.done() || r.Plan.StartedAt.IsZero() || len(active) > 0 || len(started) != len(calls) || len(r.Plan.Calls) == 0 {
		return nil, errors.New("incomplete or unbound source occurrence")
	}
	if err := validateParallelGroups(r); err != nil {
		return nil, err
	}
	pending := map[int]bool{}
	for _, c := range r.Plan.Calls {
		if c.Operation == "StartProcess" {
			if pending[c.SessionID] {
				return nil, errors.New("source session reused before exit")
			}
			pending[c.SessionID] = true
		}
		if c.Operation == "PollProcess" {
			if !pending[c.SessionID] {
				return nil, errors.New("source polls an unknown session")
			}
			if c.ExitCode != ProcessRunning {
				delete(pending, c.SessionID)
			}
		}
	}
	if len(pending) != 0 && r.Job != TerminalBench4SourceJob {
		return nil, errors.New("source includes unconsumed session handles")
	}
	r.EndingProcessSessions = slices.Sorted(maps.Keys(pending))
	return r, nil
}

func equalJSON(a, b []byte) bool {
	var left, right map[string]any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

var interceptedPatchDirs = map[string]string{
	"call_T4guAViYNCoy6x8AhUcRk0Ol": "/app",
	"call_uWoDRNhz1zCkvG2SRnl3BfZz": "/app",
	"call_8GEGWXdrXh3Lzsl1YO3USPPE": "/app",
	"call_GlxbHkDI0B4pemOVXtaQ2iJA": "/app",
	"call_VUx6ZM69llmYcf4fnTweo8Iz": "/app",
	"call_0CYorN8YDjtAiGkCSpQdTShg": "/app",
	"call_MKtTAPoiY8P1IwNrOm8EgtDl": "/app",
	"call_9WdLaxgtDzQKDwPArbzjG358": "/app",
	"call_8TwZxyyXu6LeS6IEWg3VNeAJ": "/app",
	"call_CsHHPLbgnM9SKfiDNGPVASoe": "/app",
	"call_lfoI9q8fXepl6cTe6DFdopFw": "/app",
	"call_HcjMUdXQbuhgquLSLyZGEqiH": "/app",
	"call_oUz1FMM5aO0vjrMrKvCcmpjN": "/app",
	"call_1zmklztkjVEgzl5x24uH2gEx": "/app",
	"call_WoIitjb4mDQY8viyLWdpEPxj": "/app",
	"call_2Y9eeYs4q2dPiKRS6dF9bnAS": "/app",
	"call_mhxbOXNUtUl9kxLCaRYVVK3E": "/app",
	"call_5ZxflhqiRQR6xuFjOuy848zb": "/app",
	"call_uTYuir3bN0AdKxSPlTUoVpLc": "/tmp",
	"call_pzdaK5lgqxXR6Rddv4QXXCi6": "/tmp",
	"call_STu77C7RXD6l1VwAZ99JVkbN": "/app",
	"call_T6t7JKXq1JQyKQEa4KEtiBOb": "/app",
	"call_M4n6xPzCAFt1LWL3t4DB8MxQ": "/app",
	"call_G9ew1gg3fn0dNjJbnoGs9LOK": "/app",
	"call_1mmBRZnC25qV95nuyoyhciWu": "/app",
	"call_nQ01ul1qMIfnPO9K066yLciD": "/app",
	"call_2GtFc6dT13ItHjghnp0cqs9y": "/app",
	"call_On6hZ1wLdLJ9DAyoTrFUaWJL": "/app",
	"call_yjQZF0xGnNmWWvsghae77z5A": "/app",
	"call_mgTX7OhZhGImHfqYYPPhqDEz": "/app",
	"call_v2QjdIl1wianORfHKd723J9C": "/app",
	"call_HEXtUnt6M36dD20bcLBfLDWQ": "/app",
	"call_UAXWLijmE367lu9fSoL3S0vB": "/app",
	"call_OkziUhlh3GurnJQyTZNcWsMb": "/app",
	"call_NVOMrI2qPuwyHSwp7NwRQlUe": "/app",
	"call_IE9ScyjJimnGHeZE2bkrNnb3": "/app",
	"call_bCT0sqjc6iYS56w84pr5971x": "/app",
	"call_UC3vxgQW6z62zq0FMZb4CPoj": "/app",
	"call_RTcHiO18ijJM5uSk5yxa47ZK": "/app",
}

func guestCall(c indexedCall, defaultDir string, origin, ts time.Time, sequence uint64) (replay.Call, error) {
	if c.call.Name == "write_stdin" {
		return processPoll(c, origin, ts, sequence)
	}
	if c.call.Name == "view_image" {
		return imageCall(c, origin, ts, sequence)
	}
	var args struct {
		Login        *bool   `json:"login"`
		Cmd          string  `json:"cmd"`
		Workdir      string  `json:"workdir"`
		OutputTokens int     `json:"max_output_tokens"`
		YieldMs      *uint64 `json:"yield_time_ms"`
	}
	dec := json.NewDecoder(bytes.NewReader(c.call.Args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return replay.Call{}, err
	}
	if args.Workdir == "" {
		args.Workdir = defaultDir
	}
	// These reviewed pure heredoc patches were intercepted by Codex and
	// returned the native patch tool's nested envelope instead of a shell status.
	if patchDir, intercepted := interceptedPatchDirs[c.call.ID]; intercepted {
		const responseHeader = "Wall time: 0.0000 seconds\nOutput:\nExit code: 0\nWall time: 0 seconds\nOutput:\n"
		body, ok := strings.CutPrefix(c.output, responseHeader)
		patch, opened := strings.CutPrefix(args.Cmd, "apply_patch <<'PATCH'\n")
		patch, closed := strings.CutSuffix(patch, "\nPATCH")
		if !ok || !opened || !closed || args.Workdir != patchDir {
			return replay.Call{}, errors.New("unexpected source patch delivery")
		}
		return replay.Call{Operation: "ApplyPatch", Sequence: sequence, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, Script: args.Cmd, Workdir: args.Workdir, Body: []byte(patch), ResponseStdout: []byte(body)}, nil
	}
	// Codex uses the same initial window regardless of the eventual source reply.
	yield := uint64(10000)
	if args.YieldMs != nil {
		yield = min(30000, max(250, *args.YieldMs))
	}
	login := args.Login == nil || *args.Login
	header, body, found := strings.Cut(c.output, "\nOutput:\n")
	if state := runningHeader.FindStringSubmatch(header); len(state) == 2 && found && args.Cmd != "" && path.IsAbs(args.Workdir) {
		id, err := strconv.Atoi(state[1])
		if err != nil || id == 0 {
			return replay.Call{}, errors.New("invalid running session ID")
		}
		return replay.Call{Operation: "StartProcess", SessionID: id, YieldTime: time.Duration(yield) * time.Millisecond, Sequence: sequence, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, Script: args.Cmd, Workdir: args.Workdir, Login: login, ExitCode: ProcessRunning, ResponseStdout: []byte(body)}, nil
	}
	match := exitHeader.FindStringSubmatch(header)
	if !found || len(match) != 2 || args.Cmd == "" || !path.IsAbs(args.Workdir) {
		return replay.Call{}, fmt.Errorf("step %d has no completed foreground exec or workdir", c.step)
	}
	exit, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil {
		return replay.Call{}, err
	}
	return replay.Call{Operation: "Exec", YieldTime: time.Duration(yield) * time.Millisecond, Sequence: sequence, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, Script: args.Cmd, Workdir: args.Workdir, Login: login, ExitCode: int32(exit), ResponseStdout: []byte(body)}, nil
}

func processPoll(c indexedCall, origin, ts time.Time, sequence uint64) (replay.Call, error) {
	var args struct {
		SessionID int    `json:"session_id"`
		Chars     string `json:"chars"`
		MaxTokens int    `json:"max_output_tokens"`
		YieldMs   int    `json:"yield_time_ms"`
	}
	d := json.NewDecoder(bytes.NewReader(c.call.Args))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil || args.SessionID <= 0 || args.Chars != "" || args.YieldMs < 0 {
		return replay.Call{}, errors.New("only empty, bounded native process polls are reviewed")
	}
	header, body, ok := strings.Cut(c.output, "\nOutput:\n")
	if !ok {
		return replay.Call{}, errors.New("missing poll observation")
	}
	call := replay.Call{Operation: "PollProcess", SessionID: args.SessionID, YieldTime: time.Duration(min(300000, max(5000, args.YieldMs))) * time.Millisecond, Sequence: sequence, StartOffset: ts.Sub(origin), ToolCallID: c.call.ID, ResponseStdout: []byte(body)}
	if running := runningHeader.FindStringSubmatch(header); len(running) == 2 && running[1] == strconv.Itoa(args.SessionID) {
		call.ExitCode = ProcessRunning
		return call, nil
	}
	exit := exitHeader.FindStringSubmatch(header)
	if len(exit) != 2 {
		return replay.Call{}, errors.New("missing terminal or correct running poll status")
	}
	n, err := strconv.ParseInt(exit[1], 10, 32)
	if err != nil {
		return replay.Call{}, err
	}
	call.ExitCode = int32(n)
	return call, nil
}
