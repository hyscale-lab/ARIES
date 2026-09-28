//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code, err := dispatch(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "aries-exec: %v\n", err)
	}
	os.Exit(code)
}

func dispatch(ctx context.Context, args []string, stdin, stdout, stderr *os.File) (int, error) {
	if len(args) > 0 {
		switch args[0] {
		case "--codex", "--cleanup-stage":
			return execsupervisor.RunSupervisor(ctx, args, stdin, stdout, stderr)
		case "--broker":
			return execsupervisor.RunBroker(ctx, args, stdin, stdout, stderr)
		case "--worker":
			return execsupervisor.RunWorker(ctx, args, stdin, stdout, stderr)
		}
	}
	return 125, errors.New("usage: aries-exec --codex ... | --cleanup-stage ... | --broker ... | --worker")
}
