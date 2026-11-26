// Run an external pilot harness under the native ARIES Docker observer.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/monitor"
)

func run() error {
	if len(os.Args) < 6 || os.Args[4] != "--" {
		return errors.New("usage: aries-observe RUN TASK OUTPUT -- COMMAND [ARG ...]")
	}
	runID, taskID, output := os.Args[1], os.Args[2], os.Args[3]
	source, err := docker.NewResourceSource(docker.ResourceOptions{RunID: runID, TaskIDs: []string{taskID}, DockerSocket: os.Getenv("DOCKER_HOST")})
	if err != nil {
		return err
	}
	recorder, err := monitor.New(monitor.Options{RunID: runID, TaskIDs: []string{taskID}, OutputDir: output, Source: source})
	if err != nil {
		return errors.Join(err, source.Close())
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := recorder.Start(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, os.Args[5], os.Args[6:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmdErr := cmd.Run()
	result, stopErr := recorder.Stop(context.Background())
	if stopErr == nil && result[taskID].SampleCount == 0 {
		stopErr = errors.New("no matching sandbox samples collected")
	}
	return errors.Join(cmdErr, stopErr)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
