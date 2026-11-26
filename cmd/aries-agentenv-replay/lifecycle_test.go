package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func TestInspectionRestoresLastGenerationAndRetargetsExistingSessions(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "stock.py")
	if err := os.WriteFile(helper, []byte(`import json, sys
phase, service, out, url, sandbox, snapshot, step = sys.argv[1:]
if phase == 'checkpoint':
    print('{"suspended":true}', flush=True)
    print('{"snapshot":"published"}')
elif phase == 'inspection':
    assert (sandbox, snapshot) == ('old', 'published')
    print('{"snapshot":"published","sandbox":"replacement","token":"fresh"}')
else:
    assert phase == 'audit' and sandbox == 'replacement'
    print('{}')
`), 0600); err != nil {
		t.Fatal(err)
	}
	oldHelper, oldDir := *checkpointHelper, *checkpointDir
	*checkpointHelper, *checkpointDir = helper, dir
	t.Cleanup(func() { *checkpointHelper, *checkpointDir = oldHelper, oldDir })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "fresh" || r.Header.Get("x-agentenv-sandbox-id") != "replacement" {
			t.Error("existing guest stream factory retained the old identity")
		}
		w.Header().Set("Content-Type", "application/connect+json")
		sendFrame(t, w, 0, `{"event":{"start":{"pid":100}}}`)
		sendFrame(t, w, 0, `{"event":{"end":{"exited":true,"exitCode":0}}}`)
	}))
	defer server.Close()
	g := guest{url: server.URL, sandbox: "old", token: "stale", client: server.Client()}
	s := harbor.NewCodexProcessSessions(t.Context(), g.stream)
	defer s.Close()
	l := stockLifecycle{g: &g, snapshot: "initial"}
	exits := 0
	if err := l.hooks().Checkpoint(t.Context(), 1, func() error { exits++; return nil }); err != nil || exits != 1 || !l.pending {
		t.Fatalf("checkpoint exit callbacks %d pending %t err %v", exits, l.pending, err)
	}
	if err := l.finish(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	id, err := os.ReadFile(filepath.Join(dir, "final-sandbox-id"))
	if err != nil || strings.TrimSpace(string(id)) != "replacement" || l.pending {
		t.Fatal("wrong final sandbox")
	}
	_, err = s.ObserveRelaxed(t.Context(), replay.Call{Operation: "Exec", Script: "true", YieldTime: time.Second, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
}
