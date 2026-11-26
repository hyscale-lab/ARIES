package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func sendFrame(t *testing.T, w io.Writer, flags byte, body string) {
	t.Helper()
	var hdr [5]byte
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		t.Error(err)
		return
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Error(err)
	}
}

func TestSourceHandleUsesOneGuestStream(t *testing.T) {
	finish := make(chan struct{})
	ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.Method != "POST" || req.URL.Path != "/process.Process/Start" || req.Header.Get("x-agentenv-sandbox-id") != "restored" || req.Header.Get("x-agentenv-target-port") != "49983" || req.Header.Get("X-Access-Token") != "guest-token" {
			t.Error("wrong envd route")
		}
		raw, err := io.ReadAll(req.Body)
		var request struct {
			Process processConfig
			Stdin   *bool
		}
		if err != nil || len(raw) < 5 || raw[0] != 0 || uint32(len(raw)-5) != binary.BigEndian.Uint32(raw[1:5]) || json.Unmarshal(raw[5:], &request) != nil {
			t.Error("malformed start")
			return
		}
		if request.Process.Command != "/bin/bash" || request.Process.CWD != "/app" || request.Process.Args[0] != "-lc" || request.Stdin == nil || *request.Stdin {
			t.Error("wrong process config")
		}
		w.Header().Set("Content-Type", "application/connect+json")
		sendFrame(t, w, 0, `{"event":{"start":{"pid":42}}}`)
		sendFrame(t, w, 0, `{"event":{"data":{"stdout":"b25l"}}}`)
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-ctx.Done():
			return
		}
		sendFrame(t, w, 0, `{"event":{"data":{"stderr":"dHdv"}}}`)
		sendFrame(t, w, 0, `{"event":{"end":{"exitCode":7,"exited":true,"error":"exit status 7"}}}`)
	}))
	defer server.Close()
	g := guest{url: server.URL, sandbox: "restored", token: "guest-token", client: server.Client()}
	s := harbor.NewCodexProcessSessions(ctx, g.stream)
	defer s.Close()
	c := replay.Call{Operation: "StartProcess", Script: "example", Workdir: "/app", Login: true, SessionID: 785, Sequence: 1, ExitCode: harbor.ProcessRunning, YieldTime: time.Millisecond}
	first, err := s.ObserveRelaxed(ctx, c)
	close(finish)
	if err != nil || first.ExitCode != harbor.ProcessRunning || first.Stdout != "one" || first.Timing.Milestones["command.started"].At.IsZero() {
		t.Fatalf("first = %+v, %v", first, err)
	}
	c.Operation, c.Sequence, c.ExitCode, c.YieldTime = "PollProcess", 2, 7, time.Second
	terminal, err := s.ObserveRelaxed(ctx, c)
	if err != nil || terminal.ExitCode != 7 || terminal.Stdout != "" || terminal.Stderr != "two" || calls != 1 {
		t.Fatalf("poll = %+v, %v, starts = %d", terminal, err, calls)
	}
}

func TestTransportErrorsNeverBecomeExits(t *testing.T) {
	for _, frames := range [][]string{
		{`{"event":{"data":{"stdout":"YQ=="}}}`},
		{`{"event":{"start":{"pid":1}}}`},
		{`{"event":{"start":{"pid":1}}}`, `{"event":{"end":{"exitCode":-1,"status":"signal: killed"}}}`},
	} {
		var stream bytes.Buffer
		for _, frame := range frames {
			sendFrame(t, &stream, 0, frame)
		}
		if _, err := processReply(context.Background(), &stream, io.Discard, io.Discard); err == nil {
			t.Fatal("accepted broken stream", frames)
		}
	}
}

func TestContainerWorkdirAndRead(t *testing.T) {
	for _, tc := range []struct{ runtime, wdirFlag string }{{"docker", "-w"}, {"runc", "--cwd"}} {
		g := guest{calendar: true, container: "work", runtime: tc.runtime}
		p, err := g.process(replay.Call{Operation: "Exec", Workdir: "/tmp_workspace", Script: "git status"})
		if err != nil || p.Command != tc.runtime || !reflect.DeepEqual(p.Args, []string{"exec", tc.wdirFlag, "/tmp_workspace", "work", "/bin/bash", "-c", "git status"}) || p.CWD != "" {
			t.Fatalf("%+v, %v", p, err)
		}
		p, err = g.process(replay.Call{Operation: "ReadFile", Path: "/root/USER.md"})
		if err != nil || !reflect.DeepEqual(p.Args, []string{"exec", "work", "/bin/cat", "--", "/root/USER.md"}) {
			t.Fatalf("%+v, %v", p, err)
		}
	}
}

func TestUnreviewedSourceBeforeDispatch(t *testing.T) {
	var out strings.Builder
	r := &harbor.Recording{Plan: replay.Plan{Calls: []replay.Call{{Operation: "Unsupported", ToolCallID: "example"}}}}
	if err := replayGuest(context.Background(), guest{}, r, &out); err == nil || !strings.Contains(err.Error(), "example") || out.Len() != 0 {
		t.Fatalf("output %q error %v", out.String(), err)
	}
}

func TestToolTimeCountsFirstReplyUnion(t *testing.T) {
	got, err := toolTime([]toolInterval{{10, 80}, {20, 60}, {60, 95}, {100, 110}})
	if err != nil || got != 95 {
		t.Fatalf("got %d, %v", got, err)
	}
	if _, err := toolTime([]toolInterval{{10, 0}}); err == nil {
		t.Fatal("accepted missing reply")
	}
}

func TestSelectedCodexShellEnvironment(t *testing.T) {
	call := replay.Call{Operation: "Exec", Login: true, Script: "env"}
	for _, roy := range []bool{false, true} {
		p, err := (guest{roy: roy}).process(call)
		if err != nil || p.Args[0] != "-lc" || strings.Contains(p.Args[1], "/opt/venv/bin") != roy {
			t.Fatalf("roy=%t: process=%+v err=%v", roy, p, err)
		}
	}
}
