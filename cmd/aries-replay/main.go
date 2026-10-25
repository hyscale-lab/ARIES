// Command aries-replay records a model endpoint's replies and serves them back,
// so runs on different tool sandboxes see identical model output and latency.
//
// Record, with the profile's model.base_url pointed at this server:
//
//	aries-replay -mode record -upstream https://api.deepseek.com -store recordings.jsonl
//
// Replay the same tasks later:
//
//	aries-replay -mode replay -store recordings.jsonl
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/model/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aries-replay:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "0.0.0.0:8090", "listen address; harness containers and pods must reach it")
	mode := flag.String("mode", replay.ModeReplay, "record or replay")
	upstream := flag.String("upstream", "https://api.deepseek.com", "endpoint to forward to in record mode")
	store := flag.String("store", "recordings.jsonl", "recordings file: appended in record mode, read in replay mode")
	logPath := flag.String("log", "", "append one timing line per request to this JSONL file")
	pace := flag.String("pace", replay.PaceRecorded, "replay timing: recorded reproduces the model's latency, none answers at once")
	model := flag.String("model", "", "model id /v1/models reports in replay mode")
	flag.Parse()

	handler, err := replay.New(replay.Options{Mode: *mode, Upstream: *upstream, StorePath: *store, LogPath: *logPath, Pace: *pace, Model: *model})
	if err != nil {
		return err
	}
	defer handler.Close()
	if *mode == replay.ModeReplay {
		fmt.Fprintf(os.Stderr, "aries-replay: serving %d recorded replies on %s\n", handler.Recordings(), *addr)
	} else {
		fmt.Fprintf(os.Stderr, "aries-replay: recording %s on %s\n", *upstream, *addr)
	}
	server := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
