// Command tsb-host records mapping-level host evidence for one ARIES run.
// It leaves unsupported attribution explicit and never derives accessed pages.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	runID := flag.String("run", "", "exact aries.run label")
	output := flag.String("output", "", "new JSONL output file")
	interval := flag.Duration("interval", time.Second, "minimum interval between scans")
	duration := flag.Duration("duration", 2*time.Hour, "maximum collection duration")
	flag.Parse()
	if *runID == "" || *output == "" || *interval <= 0 || *duration <= 0 {
		return errors.New("run/output and positive interval/duration required")
	}
	f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(base, *duration)
	defer cancel()
	client, e := containerd.New("/run/firecracker-containerd/containerd.sock")
	if e != nil {
		return e
	}
	defer client.Close()
	manager, e := vhive.New(vhive.Options{})
	if e != nil {
		return e
	}
	defer manager.Close()
	sequence := 0
	for {
		if ctx.Err() != nil {
			return enc.Encode(map[string]any{"schema_version": 1, "type": "collector_stopped", "time": time.Now().UTC(), "reason": ctx.Err().Error(), "scan_count": sequence})
		}
		started := time.Now().UTC()
		scanCtx, c := context.WithTimeout(namespaces.WithNamespace(ctx, "aries"), 5*time.Second)
		containers, err := client.Containers(scanCtx)
		count := 0
		if err == nil {
			for _, container := range containers {
				info, infoErr := container.Info(scanCtx)
				if infoErr != nil {
					err = errors.Join(err, infoErr)
					continue
				}
				if info.Labels["aries.managed"] != "true" || info.Labels["aries.run"] != *runID {
					continue
				}
				count++
				obs, observeErr := manager.ObserveMemory(scanCtx, info.ID)
				record := map[string]any{"schema_version": 1, "type": "host_sample", "sequence": sequence, "run_id": *runID, "task_id": info.Labels["aries.task"], "component": info.Labels["aries.component"], "runtime_id": info.ID, "status": "measured_partial", "observation": obs}
				if observeErr != nil {
					err = errors.Join(err, observeErr)
					record["status"] = "error"
					record["error"] = observeErr.Error()
				}
				if e = enc.Encode(record); e != nil {
					c()
					return e
				}
			}
		}
		c()
		summary := map[string]any{"schema_version": 1, "type": "scan", "sequence": sequence, "started_at": started, "finished_at": time.Now().UTC(), "runtime_count": count, "status": "ok", "run_id": *runID}
		if err != nil {
			summary["status"] = "error"
			summary["error"] = err.Error()
		}
		if e = enc.Encode(summary); e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		sequence++
		timer := time.NewTimer(*interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
