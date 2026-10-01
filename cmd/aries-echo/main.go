// Command aries-echo serves a deterministic OpenAI-compatible endpoint that
// replies with the request it received. Point a profile's model.base_url at it
// to inspect exactly what a harness sends.
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

	"github.com/hyscale-lab/aries/pkg/model/echo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aries-echo:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	model := flag.String("model", echo.DefaultModel, "model id served by /v1/models")
	logPath := flag.String("log", "", "append each received request to this JSONL file")
	flag.Parse()

	handler, err := echo.New(echo.Options{Model: *model, LogPath: *logPath})
	if err != nil {
		return err
	}
	defer handler.Close()
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
