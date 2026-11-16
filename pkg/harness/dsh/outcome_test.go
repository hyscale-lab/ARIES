package dsh

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
)

func TestNativeHeadlessCompletion(t *testing.T) {
	session := `{"type":"session","sessionId":"root"}` + "\n"
	start := `{"type":"status","phase":"turn_start","turn":1}` + "\n"
	end := `{"type":"status","phase":"turn_end","turn":1,"reason":{"kind":"completed"}}` + "\n"
	final := `{"type":"final","text":"Finished the change."}` + "\n"
	valid := session + start + `{"type":"tool_call","callId":"call-1","tool":"bash"}` + "\n" + `{"type":"tool_result","callId":"call-1","status":"completed"}` + "\n" + end + final
	if text, err := parseOutcome([]byte(valid)); err != nil || text != "Finished the change." {
		t.Fatalf("native completed event sequence: %q %v", text, err)
	}
	for name, events := range map[string]string{
		"missing final":     session + start + end,
		"no completed turn": session + start + final,
		"truncated":         session + start + strings.Replace(end, `"completed"`, `"max-tokens"`, 1) + final,
		"old completed":     session + start + end + `{"type":"status","phase":"turn_start","turn":2}` + "\n" + final,
		"wrong turn":        session + start + strings.Replace(end, `"turn":1`, `"turn":2`, 1) + final,
		"pending call":      session + start + `{"type":"tool_call","callId":"pending"}` + "\n" + end + final,
		"blank final":       session + start + end + `{"type":"final","text":"  "}` + "\n",
		"event after final": session + start + end + final + start,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOutcome([]byte(events)); err == nil {
				t.Fatal("accepted an incomplete native run")
			}
		})
	}
}

func TestSessionCollectionBounds(t *testing.T) {
	const content = "{\"type\":\"session\"}\n"
	for _, test := range []struct {
		name, extraName, wantError string
		size, bodyBytes            int64
	}{
		{name: "complete"},
		{name: "oversized session", extraName: "sessions/large.jsonl", size: maxEvidenceFileBytes + 1, wantError: "session \"large.jsonl\" exceeds"},
		{name: "truncated session", extraName: "sessions/truncated.jsonl", size: 4, bodyBytes: 1, wantError: "unexpected EOF"},
		{name: "oversized archive including skipped entries", extraName: "sessions/ignored.bin", size: maxSessionArchiveBytes + 1, bodyBytes: maxSessionArchiveBytes + 1, wantError: "session archive exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var prefix bytes.Buffer
			writer := tar.NewWriter(&prefix)
			if err := writer.WriteHeader(&tar.Header{Name: "sessions/project/root.jsonl", Mode: 0600, Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(writer, content); err != nil {
				t.Fatal(err)
			}
			if test.extraName != "" {
				if err := writer.WriteHeader(&tar.Header{Name: test.extraName, Mode: 0600, Size: test.size}); err != nil {
					t.Fatal(err)
				}
			} else if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			archive := io.MultiReader(&prefix, io.LimitReader(zeroReader{}, test.bodyBytes))
			manager := &Manager{runtime: &harness.Runtime{Options: harness.RuntimeOptions{Deployment: sessionDeployment{archive: archive}}}}
			active := &harness.Occurrence{
				ID: "dsh-test", Credentials: harness.NewCredentials("DSH"),
				Artifacts: &harness.Artifacts{Directory: t.TempDir()},
			}
			err := manager.collectSessions(context.Background(), active)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("collection error = %v, want %q", err, test.wantError)
			}
			saved, err := os.ReadFile(filepath.Join(active.Artifacts.Directory, "sessions/project/root.jsonl"))
			if err != nil || string(saved) != content || len(active.Artifacts.Paths()) != 1 {
				t.Fatalf("complete evidence was lost or partial evidence saved: content=%q paths=%v error=%v", saved, active.Artifacts.Paths(), err)
			}
		})
	}
}

type sessionDeployment struct {
	deployment.Deployment
	archive io.Reader
}

func (d sessionDeployment) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	return io.NopCloser(d.archive), deployment.FileInfo{}, nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
