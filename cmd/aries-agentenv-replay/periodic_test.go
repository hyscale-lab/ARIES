package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func TestStockReplayWaitsForBoundaryReceipts(t *testing.T) {
	dir, err := os.MkdirTemp("", "stock-hook-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "b.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	errs := make(chan error, 1)
	go func() {
		for _, phase := range []string{"session-start", "session-end"} {
			conn, err := listener.Accept()
			if err != nil {
				errs <- err
				return
			}
			var raw json.RawMessage
			err = json.NewDecoder(conn).Decode(&raw)
			if err == nil && string(raw) != fmt.Sprintf("[%q,\"sandbox\",0,\"\",[%q]]", phase, phase) {
				err = fmt.Errorf("unexpected request %s", raw)
			}
			if err == nil {
				now, _ := harbor.Clock()
				err = json.NewEncoder(conn).Encode(map[string]int64{"pause_ack_ns": now, "resume_request_ns": now})
			}
			conn.Close()
			if err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()
	prev := *boundarySocket
	*boundarySocket = path
	defer func() { *boundarySocket = prev }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := replayGuest(ctx, guest{sandbox: "sandbox"}, &harbor.Recording{}, &out); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if bytes.Count(out.Bytes(), []byte(`"event":"trace_capture"`)) != 2 || !bytes.Contains(out.Bytes(), []byte(`"event":"session_end"`)) {
		t.Fatal(out.String())
	}
}
