// Execute the sampling process through the replay's restored guest task service.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 7 {
		return fmt.Errorf("usage: cg-exec SOCKET CHECKPOINT_DIR SHIM_DIR IO_DIR RUNTIME_ID GUEST_SCRIPT")
	}
	options := vhive.Options{
		Socket: os.Args[1], CheckpointDir: os.Args[2], ShimDir: os.Args[3], IODir: os.Args[4],
		Namespace: "aries", Snapshotter: "devmapper", IdleMode: "checkpoint",
		GuestTaskV3: true, VCPUs: 1, MemoryMB: 8192,
	}
	manager, err := vhive.New(options)
	if err != nil {
		return err
	}
	defer manager.Close()
	script, err := os.ReadFile(os.Args[6])
	if err != nil {
		return err
	}
	result, err := manager.ExecStream(context.Background(), os.Args[5], core.Command{
		Path: os.Getenv("ZB_GUEST_PYTHON"), Args: []string{"-I", "-S", "-u", "-c", string(script)},
	}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("guest cgroup sampler exited %d", result.ExitCode)
	}
	return nil
}
