package main

import (
	"context"
	"flag"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

var boundarySocket = flag.String("boundary-socket", "", "worker-local stock full-memory observer socket")

func stockPeriodic(path, sandbox string) func(context.Context, string, int, []string) (harbor.Freeze, error) {
	if path == "" {
		return nil
	}
	return harbor.PeriodicSocket(path, sandbox, "")
}
