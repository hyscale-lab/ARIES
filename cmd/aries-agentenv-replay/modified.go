package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func modifiedV5(ctx context.Context, path string, g guest, r *harbor.Recording) (retErr error) {
	var c struct {
		Options                          vhive.Options
		Image, Fixture, Sampler, Samples string
		Order                            int
		Network                          bool
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.Options.MemoryMB != 4096 || c.Options.VCPUs != 2 || !c.Options.NativeCheckpoint || c.Options.CheckpointAfterExec || c.Options.IdleMode != "checkpoint" || (c.Order != 1 && c.Order != 6) || c.Image == "" || c.Fixture == "" || c.Sampler == "" || c.Samples == "" {
		return errors.New("invalid modified v5 study configuration")
	}
	m, err := vhive.New(c.Options)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	name := fmt.Sprintf("aries-wcb-%d", time.Now().UnixNano())
	log := json.NewEncoder(os.Stdout)
	event := func(phase string) error {
		t, e := harbor.Clock()
		if e != nil {
			return e
		}
		return log.Encode(map[string]any{"event": phase, "monotonic_ns": t, "runtime_id": name, "task": r.Task, "image": c.Image, "vcpus": 2, "memory_mib": 4096, "reporting_order_requested": c.Order})
	}
	if err = event("create_start"); err != nil {
		return err
	}
	req := deployment.Request{Name: name, Image: c.Image, AllowNetwork: c.Network, StorageMB: 65536, Workdir: "/tmp_workspace", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": name, "aries.task": r.Task}}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			if e := m.Stop(cleanup, id); e != nil {
				retErr = errors.Join(retErr, e)
			} else {
				retErr = errors.Join(retErr, event("removed"))
			}
		}()
	}
	if err != nil {
		return err
	}
	if err = m.Validate(ctx, id, req, nil); err != nil {
		return err
	}
	f, err := os.Open(c.Fixture)
	if err != nil {
		return err
	}
	// Verify the complete fixture before extraction. The native task transport on
	// this runtime can close a short stdin stream before the guest reads it.
	hash := sha256.New()
	buf := make([]byte, 48*1024)
	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			hash.Write(buf[:n])
			res, e := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-ec", `printf %s "$1" | base64 -d >> /tmp/zb-fixture.tar`, "sh", base64.StdEncoding.EncodeToString(buf[:n])}})
			if e != nil || res.ExitCode != 0 {
				return errors.Join(e, f.Close(), fmt.Errorf("stage fixture: exit %d %s", res.ExitCode, res.Stderr))
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return errors.Join(readErr, f.Close())
		}
	}
	if err = f.Close(); err != nil {
		return err
	}
	checked := func(phase string, command core.Command) error {
		res, e := m.Exec(ctx, id, command)
		if e != nil {
			return e
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: exit %d: %s %s", phase, res.ExitCode, res.Stderr, res.Stdout)
		}
		return log.Encode(map[string]any{"event": phase, "stdout": res.Stdout})
	}
	if err = checked("fixture_verified", core.Command{Path: "/bin/sh", Args: []string{"-ec", `printf '%s  /tmp/zb-fixture.tar\n' "$1" | sha256sum -c -; tar xf /tmp/zb-fixture.tar -C /; rm /tmp/zb-fixture.tar`, "sh", fmt.Sprintf("%x", hash.Sum(nil))}}); err != nil {
		return err
	}
	if err = checked("fixture_prepared", core.Command{Path: "/bin/sh", Args: []string{"/tmp/zb-prepare.sh"}}); err != nil {
		return err
	}
	if err = m.Start(ctx, id); err != nil {
		return err
	}
	for _, phase := range []string{"snapshot_start", "snapshot_complete", "restore_start", "restore_complete"} {
		if err = event(phase); err != nil {
			return err
		}
		switch phase {
		case "snapshot_start":
			err = m.Suspend(ctx, id)
		case "restore_start":
			err = m.Resume(ctx, id)
		}
		if err != nil {
			return err
		}
	}
	if err = checked("restored_reclamation", core.Command{Path: "/bin/sh", Args: []string{"-ec", fmt.Sprintf("o=$(cat /sys/module/page_reporting/parameters/page_reporting_order); test \"$o\" = %d; echo order=$o; for f in /sys/module/damon_reclaim/parameters/*; do test -r \"$f\" && echo \"$f=$(cat \"$f\")\"; done", c.Order)}}); err != nil {
		return err
	}
	g.sandbox = id
	g.vmmSocket, err = m.VMMSocket(ctx, id)
	if err != nil {
		return err
	}
	g.direct = func(ctx context.Context, p processConfig, timeout time.Duration, out, stderr io.Writer) (core.CommandResult, error) {
		return m.ExecStream(ctx, id, core.Command{Path: p.Command, Args: p.Args, Dir: p.CWD, Timeout: timeout}, nil, out, stderr)
	}
	stop, err := harbor.StartVMMSampler(ctx, c.Sampler, g.vmmSocket, c.Samples)
	if err != nil {
		return err
	}
	if err = event("sampler_ready"); err == nil {
		err = replayGuest(ctx, g, r, os.Stdout)
	}
	if err = errors.Join(err, stop()); err != nil {
		return err
	}
	return checked("result_files", core.Command{Path: "/bin/sh", Args: []string{"/tmp/zb-result.sh"}})
}
