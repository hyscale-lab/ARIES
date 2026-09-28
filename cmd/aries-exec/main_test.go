//go:build linux

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/internal/execsupervisor"
)

func TestDispatchRejectsMissingOrUnknownMode(t *testing.T) {
	for _, args := range [][]string{nil, {"--unknown"}, {"codex"}, {"--codex=other"}} {
		code, err := dispatch(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
		if code != 125 || err == nil || !strings.HasPrefix(err.Error(), "usage: aries-exec ") {
			t.Fatalf("dispatch(%q) = (%d, %v)", args, code, err)
		}
	}
}

func TestDispatchPreservesEachModeArgumentValidation(t *testing.T) {
	// Malformed invocations exercise routing without changing this test
	// process's subreaper, credential, or inherited-descriptor state.
	for _, test := range []struct {
		name string
		args []string
		want func(context.Context, []string) (int, error)
	}{
		{name: "Codex", args: []string{"--codex"}, want: func(ctx context.Context, args []string) (int, error) {
			return execsupervisor.RunSupervisor(ctx, args, os.Stdin, os.Stdout, os.Stderr)
		}},
		{name: "Codex cleanup", args: []string{"--cleanup-stage"}, want: func(ctx context.Context, args []string) (int, error) {
			return execsupervisor.RunSupervisor(ctx, args, os.Stdin, os.Stdout, os.Stderr)
		}},
		{name: "broker", args: []string{"--broker", "--unexpected"}, want: func(ctx context.Context, args []string) (int, error) {
			return execsupervisor.RunBroker(ctx, args, os.Stdin, os.Stdout, os.Stderr)
		}},
		{name: "worker", args: []string{"--worker", "--unexpected"}, want: func(ctx context.Context, args []string) (int, error) {
			return execsupervisor.RunWorker(ctx, args, os.Stdin, os.Stdout, os.Stderr)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			wantCode, wantErr := test.want(ctx, test.args)
			if wantCode == 0 || wantErr == nil {
				t.Fatal("invalid fixture arguments were accepted")
			}
			code, err := dispatch(ctx, test.args, os.Stdin, os.Stdout, os.Stderr)
			if code != wantCode || err == nil || err.Error() != wantErr.Error() {
				t.Fatalf("dispatch = (%d, %v), want (%d, %v)", code, err, wantCode, wantErr)
			}
		})
	}
}
