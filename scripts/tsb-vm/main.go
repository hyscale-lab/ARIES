// Command tsb-vm runs one calibration fixture in a fixed-capacity persistent VM.
// It is not an agent harness and never reads benchmark verifier inputs.
package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	fcclient "github.com/firecracker-microvm/firecracker-containerd/firecracker-control/client"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (retErr error) {
	fixture := flag.String("fixture", "", "static guest calibration executable")
	output := flag.String("output", "", "new private artifact directory")
	image := flag.String("image", "docker.io/library/alpine:3.21.3", "preimported image")
	timeout := flag.Duration("timeout", 20*time.Minute, "whole calibration deadline")
	flag.Parse()
	if *fixture == "" || *output == "" {
		return errors.New("-fixture and -output required")
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return err
	}
	write := func(name string, v any) error {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(*output, name), append(b, '\n'), 0600)
	}
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(base, *timeout)
	defer cancel()
	m, err := vhive.New(vhive.Options{VCPUs: 2, MemoryMB: 4096, IdleMode: "persistent"})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	cpu, mem := 2.0, 4096
	name := fmt.Sprintf("aries-tsb-calibration-%d", time.Now().UnixNano())
	req := deployment.Request{Name: name, Image: *image, Workdir: "/", CPU: &cpu, MemoryMB: &mem, StorageMB: 10240, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "tsb-calibration", "aries.task": "calibration"}}
	if err = write("request.json", req); err != nil {
		return err
	}
	id, createErr := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 2*time.Minute)
			defer c()
			e := m.Stop(cleanCtx, id)
			status := "confirmed"
			msg := ""
			if e != nil {
				status = "failed"
				msg = e.Error()
			}
			retErr = errors.Join(retErr, e, write("cleanup.json", map[string]any{"runtime_id": id, "status": status, "error": msg, "time": time.Now().UTC()}))
		}()
	}
	if createErr != nil {
		return createErr
	}
	if err = m.Validate(ctx, id, req, nil); err != nil {
		return err
	}
	if err = m.Start(ctx, id); err != nil {
		return err
	}
	control, err := fcclient.New("/run/firecracker-containerd/containerd.sock.ttrpc")
	if err != nil {
		return err
	}
	defer control.Close()
	info, err := control.GetVMInfo(namespaces.WithNamespace(ctx, "aries"), &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return err
	}
	if err = write("vm-info.json", info); err != nil {
		return err
	}
	// Calibration only: the deterministic fixture needs pagemap PFNs.
	// This modifies only this owned calibration container, never benchmark requests.
	native, err := containerd.New("/run/firecracker-containerd/containerd.sock")
	if err != nil {
		return err
	}
	defer native.Close()
	nativeCtx := namespaces.WithNamespace(ctx, "aries")
	container, err := native.LoadContainer(nativeCtx, id)
	if err != nil {
		return err
	}
	spec, err := container.Spec(nativeCtx)
	if err != nil {
		return err
	}
	caps := spec.Process.Capabilities
	caps.Bounding = append(caps.Bounding, "CAP_SYS_ADMIN")
	caps.Effective = append(caps.Effective, "CAP_SYS_ADMIN")
	caps.Permitted = append(caps.Permitted, "CAP_SYS_ADMIN")
	if err = container.Update(nativeCtx, containerd.UpdateContainerOpts(containerd.WithSpec(spec))); err != nil {
		return err
	}
	if err = write("calibration-capabilities.json", map[string]any{"added": "CAP_SYS_ADMIN", "scope": "deterministic calibration fixture only; not benchmark", "reason": "guest pagemap physical slot identification"}); err != nil {
		return err
	}
	guest, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", "uname -r; cat /proc/sys/kernel/random/boot_id; getconf _NPROCESSORS_ONLN; cat /proc/meminfo; cat /proc/cmdline; grep Cap /proc/self/status; test -e /sys/kernel/mm/page_idle/bitmap && echo GUEST_IDLE_AVAILABLE"}})
	if err != nil {
		return err
	}
	if err = write("guest.json", guest); err != nil {
		return err
	}
	f, err := os.Open(*fixture)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(w)
		e := tw.WriteHeader(&tar.Header{Name: "tsb-calibration-fixture", Mode: 0700, Size: st.Size()})
		if e == nil {
			_, e = io.Copy(tw, f)
		}
		e = errors.Join(e, tw.Close())
		_ = w.CloseWithError(e)
		done <- e
	}()
	uploadErr := m.UploadArchive(ctx, id, "/tmp", r)
	_ = r.CloseWithError(uploadErr)
	if err = errors.Join(uploadErr, <-done); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "calibration VM ready:", id, "metadata:", *output)
	res, err := m.ExecStream(ctx, id, core.Command{Path: "/tmp/tsb-calibration-fixture", Args: flag.Args()}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("fixture exited %d", res.ExitCode)
	}
	return nil
}
