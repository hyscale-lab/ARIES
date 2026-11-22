// Run a standalone file calibration over an actual vHive checkpoint restore.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

func run(ctx context.Context) (retErr error) {
	var opts vhive.Options
	opts.IdleMode = "checkpoint"
	opts.MemoryMB = 2048
	opts.VCPUs = 1
	opts.GuestTaskV3 = true
	flag.StringVar(&opts.Socket, "socket", "", "isolated containerd socket")
	flag.StringVar(&opts.Namespace, "namespace", "aries", "containerd namespace")
	flag.StringVar(&opts.CheckpointDir, "checkpoint-dir", "", "isolated checkpoint directory")
	flag.StringVar(&opts.ShimDir, "shim-dir", "", "isolated shim directory")
	flag.StringVar(&opts.IODir, "io-dir", "", "isolated I/O directory")
	flag.BoolVar(&opts.NativeCheckpoint, "native-checkpoint", false, "use the optimized native checkpoint API")
	flag.BoolVar(&opts.NativeReportingOnly, "native-reporting-only", false, "enable native reporting without DAMON")
	image := flag.String("image", "", "prepared benchmark image for an idle guest task")
	hook := flag.String("hook", "", "host file-backing diagnostic observer")
	flag.Parse()
	if *hook == "" || *image == "" {
		return errors.New("image and hook are required")
	}
	manager, err := vhive.New(opts)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, manager.Close()) }()
	name := fmt.Sprintf("aries-filecal-%d", time.Now().UnixNano())
	req := deployment.Request{
		Name: name, Image: *image, StorageMB: 10240, Workdir: "/app",
		Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "task-container",
			"aries.component": "sandbox", "aries.run": name, "aries.task": "file-calibration",
		},
	}
	id, err := manager.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			retErr = errors.Join(retErr, manager.Stop(cleanup, id))
		}()
	}
	if err != nil {
		return err
	}
	if err := manager.Start(ctx, id); err != nil {
		return err
	}
	observe := func(action string) error {
		socket, err := manager.VMMSocket(ctx, id)
		if err != nil {
			return err
		}
		command := exec.CommandContext(ctx, *hook, action, id, strings.TrimPrefix(socket, "unix://"))
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		return command.Run()
	}
	if err := observe("prepare"); err != nil {
		return err
	}
	if err := manager.Suspend(ctx, id); err != nil {
		return err
	}
	if err := manager.Resume(ctx, id); err != nil {
		return err
	}
	return observe("measure")
}

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
