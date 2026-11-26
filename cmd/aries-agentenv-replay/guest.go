package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

type guest struct {
	url, sandbox, token, container, runtime string
	openClaw, roy                           bool
	shell                                   string
	client                                  *http.Client
}

type processConfig struct {
	Command string   `json:"cmd"`
	Args    []string `json:"args"`
	CWD     string   `json:"cwd,omitempty"`
}

func (g guest) process(c replay.Call) (processConfig, error) {
	var p processConfig
	switch c.Operation {
	case "Exec", "StartProcess":
		flag := "-c"
		script := c.Script
		if !g.openClaw && c.Login {
			flag = "-lc"
		}
		if g.roy {
			// Match the reviewed Roy image's shell environment in aries-harbor.
			script = `export PATH=/opt/venv/bin:/root/.local/bin:"$PATH"; ` + script
		}
		shell := g.shell
		if shell == "" {
			shell = "/bin/bash"
		}
		p = processConfig{shell, []string{flag, script}, c.Workdir}
	case "ApplyPatch":
		const binary = "/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
		p = processConfig{binary, []string{"--codex-run-as-apply-patch", string(c.Body)}, c.Workdir}
	case "ViewImage":
		p = processConfig{"/usr/local/bin/aries-harbor-image", []string{c.Path, strconv.Itoa(c.ImageWidth), strconv.Itoa(c.ImageHeight)}, ""}
	case "OpenClawImage":
		if !g.openClaw {
			return p, errors.New("unexpected model image operation")
		}
		p = processConfig{"node", []string{"/tmp/paper-image.mjs", string(c.Body)}, ""}
	case "WriteFile", "EditFile":
		if !g.openClaw {
			return p, errors.New("unexpected OpenClaw document operation")
		}
		p = processConfig{"node", []string{"-e", documentOperation, c.Operation, string(c.Body)}, ""}
	case "ReadFile":
		if !g.openClaw {
			return p, fmt.Errorf("unexpected Codex file read for %s", c.ToolCallID)
		}
		p = processConfig{"/bin/cat", []string{"--", c.Path}, ""}
		if c.ResponseCode == "ENOENT" {
			p = processConfig{"node", []string{"-e", missingFileRead, c.Path}, ""}
		} else if c.ResponseCode == "image/png" {
			p = processConfig{"/usr/bin/base64", []string{"-w0", "--", c.Path}, ""}
		}
	default:
		return p, fmt.Errorf("unsupported guest operation %q for %s", c.Operation, c.ToolCallID)
	}
	if g.container != "" {
		args := []string{"exec"}
		if p.CWD != "" {
			if g.runtime == "runc" {
				args = append(args, "--cwd", p.CWD)
			} else {
				args = append(args, "-w", p.CWD)
			}
		}
		args = append(args, g.container, p.Command)
		p = processConfig{g.runtime, append(args, p.Args...), ""}
	}
	return p, nil
}

// envd's Connect Process/Start handler supports JSON and protobuf on the same route.
func (g *guest) stream(ctx context.Context, c replay.Call, stdout, stderr io.Writer) (core.CommandResult, error) {
	var result core.CommandResult
	p, err := g.process(c)
	if err != nil {
		return result, err
	}
	payload, err := json.Marshal(struct {
		Process processConfig `json:"process"`
		Stdin   bool          `json:"stdin"`
	}{p, false})
	if err != nil {
		return result, err
	}
	frame := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	req, err := http.NewRequestWithContext(ctx, "POST", g.url+"/process.Process/Start", bytes.NewReader(append(frame, payload...)))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-agentenv-sandbox-id", g.sandbox)
	req.Header.Set("x-agentenv-target-port", "49983")
	if g.token != "" {
		req.Header.Set("X-Access-Token", g.token)
	}
	if c.Timeout > 0 {
		req.Header.Set("Connect-Timeout-Ms", strconv.FormatInt(c.Timeout.Milliseconds(), 10))
	}
	req.SetBasicAuth("root", "")
	resp, err := g.client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, errors.Join(fmt.Errorf("envd returned %s: %s", resp.Status, strings.TrimSpace(string(msg))), err)
	}
	if typ, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); typ != "application/connect+json" {
		return result, fmt.Errorf("unexpected envd content type %q", typ)
	}
	if g.openClaw && (c.Operation == "Exec" || c.Operation == "StartProcess") {
		stderr = stdout // OpenClaw returns aggregated output in stream order.
	}
	return processReply(ctx, resp.Body, stdout, stderr)
}

type processEvent struct {
	Start *struct{ PID uint32 }             `json:"start"`
	Data  *struct{ Stdout, Stderr *[]byte } `json:"data"`
	End   *struct {
		ExitCode      int
		Exited        bool
		Status, Error string
	} `json:"end"`
	Keepalive *struct{} `json:"keepalive"`
}

func processReply(ctx context.Context, in io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	var result core.CommandResult
	var header [5]byte
	started := false
	for {
		if _, err := io.ReadFull(in, header[:]); err != nil {
			return result, fmt.Errorf("envd stream ended before process exit: %w", err)
		}
		size := binary.BigEndian.Uint32(header[1:])
		if size > 16<<20 {
			return result, fmt.Errorf("oversized envd envelope: %d bytes", size)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(in, body); err != nil {
			return result, err
		}
		switch header[0] {
		case 2:
			return result, fmt.Errorf("envd stream closed before process exit: %s", body)
		case 0:
		default:
			return result, fmt.Errorf("unsupported envd envelope flags %d", header[0])
		}
		var msg struct{ Event processEvent }
		if err := json.Unmarshal(body, &msg); err != nil {
			return result, err
		}
		e := msg.Event
		switch {
		case e.Start != nil && !started && e.Start.PID != 0:
			started = true
			core.MarkCommandTiming(ctx, "command.started")
		case !started:
			return result, errors.New("envd event before process start")
		case e.Data != nil:
			var err error
			switch {
			case e.Data.Stdout != nil:
				_, err = stdout.Write(*e.Data.Stdout)
			case e.Data.Stderr != nil:
				_, err = stderr.Write(*e.Data.Stderr)
			default:
				return result, errors.New("unexpected envd output in non-PTY stream")
			}
			if err != nil {
				return result, err
			}
		case e.End != nil:
			core.MarkCommandTiming(ctx, "command.exited")
			if !e.End.Exited || e.End.ExitCode < 0 {
				return result, fmt.Errorf("abnormal guest process termination: %s %s", e.End.Status, e.End.Error)
			}
			result.ExitCode = e.End.ExitCode
			return result, nil
		case e.Keepalive != nil:
		default:
			return result, errors.New("unexpected envd process event")
		}
	}
}
