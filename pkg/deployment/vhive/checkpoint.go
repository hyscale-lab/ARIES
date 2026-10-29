package vhive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/sys/unix"
	pb "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"strconv"
)

type snapshotControl interface {
	PauseVM(context.Context, *proto.PauseVMRequest) (*emptypb.Empty, error)
	CreateSnapshot(context.Context, *proto.CreateSnapshotRequest) (*emptypb.Empty, error)
}

type checkpointState struct {
	TaskRunning bool                   `json:"task_running"`
	Phase       string                 `json:"phase"`
	Request     *proto.CreateVMRequest `json:"request"`
	Memory      string                 `json:"memory,omitempty"`
	Snapshot    string                 `json:"snapshot,omitempty"`
	Processes   []checkpointProcess    `json:"processes,omitempty"`
}

type checkpointProcess struct {
	PID   int
	Start string
}

func (m *Manager) runtimeDir(id string) string {
	return filepath.Join(m.options.CheckpointDir, m.options.Namespace, id)
}
func (m *Manager) saveCheckpoint(id string, s *checkpointState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := m.runtimeDir(id)
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, we := f.Write(data)
	if err = errors.Join(we, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func (m *Manager) loadCheckpoint(id string) (*checkpointState, error) {
	if !identifier.MatchString(id) {
		return nil, errors.New("invalid checkpoint identity")
	}
	data, err := os.ReadFile(filepath.Join(m.runtimeDir(id), "state.json"))
	if err != nil {
		return nil, err
	}
	var s checkpointState
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Request == nil || s.Request.VMID != id {
		return nil, errors.New("checkpoint VM identity differs")
	}
	return &s, nil
}

// flock coordinates the Runner and its independent bridge process. Shared
// holders may execute concurrently; only the last completed call parks the VM.
func (m *Manager) checkpointLock(ctx context.Context, id string, exclusive, nonblock bool) (*os.File, error) {
	if !identifier.MatchString(id) {
		return nil, errors.New("invalid checkpoint identity")
	}
	f, err := os.OpenFile(filepath.Join(m.runtimeDir(id), "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != unix.EWOULDBLOCK || nonblock {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, context.Cause(ctx)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func unlockCheckpoint(f *os.File) {
	if f != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}
}

func (m *Manager) acquireCheckpoint(ctx context.Context, id string) (*os.File, error) {
	f, err := m.checkpointLock(ctx, id, false, false)
	if err != nil {
		return nil, err
	}
	s, err := m.loadCheckpoint(id)
	if err != nil {
		unlockCheckpoint(f)
		return nil, err
	}
	if s.Phase == "running" {
		return f, nil
	}
	unlockCheckpoint(f)
	f, err = m.checkpointLock(ctx, id, true, false)
	if err != nil {
		return nil, err
	}
	if err = m.resumeCheckpoint(ctx, id); err != nil {
		unlockCheckpoint(f)
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil {
		unlockCheckpoint(f)
		return nil, err
	}
	s, err = m.loadCheckpoint(id)
	if err != nil {
		unlockCheckpoint(f)
		return nil, err
	}
	if s.Phase != "running" {
		unlockCheckpoint(f)
		return m.acquireCheckpoint(ctx, id)
	}
	return f, nil
}

func (m *Manager) execCheckpoint(ctx context.Context, id string, c core.Command, in io.Reader, out, stderr io.Writer) (core.CommandResult, error) {
	f, err := m.acquireCheckpoint(ctx, id)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	result, execErr := m.execGuestStream(ctx, id, c, in, out, stderr)
	unlockCheckpoint(f)
	if m.options.CheckpointAfterExec && execErr == nil {
		f, err = m.checkpointLock(ctx, id, true, true)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return result, execErr
		}
		if err == nil {
			err = m.suspendCheckpoint(ctx, id)
			unlockCheckpoint(f)
		}
		execErr = errors.Join(execErr, err)
	}
	return result, execErr
}

// Suspend checkpoints a quiescent VM to disk and confirms the VMM and shim
// exited. It retains the logical sandbox, active disk device and CNI allocation.
func (m *Manager) Suspend(ctx context.Context, id string) error {
	if m.options.IdleMode != "checkpoint" {
		return errors.New("suspend requires checkpoint idle_mode")
	}
	f, err := m.checkpointLock(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer unlockCheckpoint(f)
	return m.suspendCheckpoint(m.ctx(ctx), id)
}
func (m *Manager) Resume(ctx context.Context, id string) error {
	if m.options.IdleMode != "checkpoint" {
		return errors.New("resume requires checkpoint idle_mode")
	}
	f, err := m.checkpointLock(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer unlockCheckpoint(f)
	return m.resumeCheckpoint(m.ctx(ctx), id)
}
func (m *Manager) suspendCheckpoint(ctx context.Context, id string) error {
	ctx = m.ctx(ctx)
	s, err := m.loadCheckpoint(id)
	if err != nil {
		return err
	}
	if s.Phase == "suspended" {
		return nil
	}
	if s.Phase != "running" {
		return fmt.Errorf("cannot suspend checkpoint in %s state", s.Phase)
	}
	control, err := m.vmControl()
	if err != nil {
		return err
	}
	snap, ok := control.(snapshotControl)
	if !ok {
		return errors.New("native VM control lacks snapshot operations")
	}
	running, err := m.guestTaskRunning(ctx, id)
	if err != nil {
		return err
	}
	s.TaskRunning = running
	processes, err := m.checkpointProcesses(ctx, id)
	if err != nil {
		return err
	}
	// Each generation has fresh paths: never overwrite memory backing a resumed VM.
	dir, err := os.MkdirTemp(m.runtimeDir(id), "snapshot-")
	if err != nil {
		return err
	}
	s.Memory = filepath.Join(dir, "memory")
	s.Snapshot = filepath.Join(dir, "vmstate")
	s.Processes = processes
	s.Phase = "pausing"
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	if _, err = snap.PauseVM(ctx, &proto.PauseVMRequest{VMID: id}); err != nil {
		return err
	}
	s.Phase = "paused"
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	if _, err = snap.CreateSnapshot(ctx, &proto.CreateSnapshotRequest{VMID: id, MemFilePath: s.Memory, SnapshotPath: s.Snapshot}); err != nil {
		return err
	}
	for _, p := range []string{s.Memory, s.Snapshot} {
		if err = os.Chmod(p, 0600); err != nil {
			return err
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		if e = errors.Join(f.Sync(), f.Close()); e != nil {
			return e
		}
	}
	s.Phase = "stopping"
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	if err = m.stopCheckpointProcesses(ctx, id, s); err != nil {
		return err
	}
	s.Phase = "suspended"
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	entries, readErr := os.ReadDir(m.runtimeDir(id))
	if readErr != nil {
		return readErr
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "snapshot-") && filepath.Join(m.runtimeDir(id), entry.Name()) != dir {
			if err = os.RemoveAll(filepath.Join(m.runtimeDir(id), entry.Name())); err != nil {
				return err
			}
		}
	}
	m.options.Logger.WithField("vm", id).Info("vHive microVM checkpointed to disk")
	return nil
}
func (m *Manager) resumeCheckpoint(ctx context.Context, id string) error {
	s, err := m.loadCheckpoint(id)
	if err != nil {
		return err
	}
	if s.Phase == "running" {
		return nil
	}
	if s.Phase != "suspended" {
		return fmt.Errorf("cannot restore checkpoint in %s state; cleanup required", s.Phase)
	}
	if err = confirmCheckpointProcesses(s.Processes); err != nil {
		return err
	}
	request := pb.Clone(s.Request).(*proto.CreateVMRequest)
	request.LoadSnapshot = true
	request.SnapshotPath = s.Snapshot
	request.MemBackend = &proto.MemoryBackend{BackendType: "File", BackendPath: s.Memory}
	mounts, err := m.client.SnapshotService(m.options.Snapshotter).Mounts(m.ctx(ctx), id)
	if err != nil {
		return err
	}
	if len(mounts) != 1 {
		return errors.New("checkpoint requires one container disk")
	}
	request.ContainerSnapshotPath = mounts[0].Source
	// Mark before submitting: an ambiguous native response must never replay a VM.
	s.Phase = "restoring"
	// The previous VMM and shim have exited; a failed restore may own new
	// processes whose absence must be established independently during cleanup.
	s.Processes = nil
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	control, err := m.vmControl()
	if err != nil {
		return err
	}
	if _, err = control.CreateVM(m.ctx(ctx), request); err != nil {
		return fmt.Errorf("restore vHive microVM: %w", err)
	}
	s.Phase = "running"
	s.Processes = nil
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	m.options.Logger.WithField("vm", id).Info("vHive microVM restored")
	return nil
}
func (m *Manager) checkpointRunning(ctx context.Context, id string) (bool, error) {
	_, err := m.client.LoadContainer(m.ctx(ctx), id)
	if err != nil {
		return false, err
	}
	f, err := m.checkpointLock(ctx, id, false, false)
	if err != nil {
		return false, err
	}
	defer unlockCheckpoint(f)
	s, err := m.loadCheckpoint(id)
	if err != nil {
		return false, err
	}
	if s.Phase == "suspended" {
		return s.TaskRunning, confirmCheckpointProcesses(s.Processes)
	}
	if s.Phase != "running" {
		return false, fmt.Errorf("checkpoint in %s state", s.Phase)
	}
	return m.guestTaskRunning(m.ctx(ctx), id)
}

func processStart(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("checkpoint process verification requires host PID namespace")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	index := strings.LastIndex(string(data), ") ")
	if index < 0 {
		return "", errors.New("invalid process identity")
	}
	rest := string(data)[index+2:]
	fields := strings.Fields(rest)
	if len(fields) < 20 {
		return "", errors.New("invalid process identity")
	}
	return fields[19], nil
}
func socketProcess(ctx context.Context, path string) (checkpointProcess, error) {
	shortPath, closePath, err := shortUnixSocketPath(strings.TrimPrefix(path, "unix://"))
	if err != nil {
		return checkpointProcess{}, err
	}
	defer closePath()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", shortPath)
	if err != nil {
		return checkpointProcess{}, err
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return checkpointProcess{}, err
	}
	var cred *unix.Ucred
	var ce error
	if err = raw.Control(func(fd uintptr) { cred, ce = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return checkpointProcess{}, err
	}
	if ce != nil {
		return checkpointProcess{}, ce
	}
	start, err := processStart(int(cred.Pid))
	return checkpointProcess{int(cred.Pid), start}, err
}
func (m *Manager) checkpointProcesses(ctx context.Context, id string) ([]checkpointProcess, error) {
	control, err := m.vmControl()
	if err != nil {
		return nil, err
	}
	info, err := control.GetVMInfo(m.ctx(ctx), &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return nil, err
	}
	vm, err := socketProcess(ctx, info.SocketPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", vm.PID))
	if err != nil {
		return nil, err
	}
	index := strings.LastIndex(string(data), ") ")
	if index < 0 {
		return nil, errors.New("invalid VMM process identity")
	}
	fields := strings.Fields(string(data)[index+2:])
	if len(fields) < 2 {
		return nil, errors.New("invalid VMM parent identity")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, err
	}
	start, err := processStart(parent)
	if err != nil {
		return nil, err
	}
	return []checkpointProcess{vm, {PID: parent, Start: start}}, nil
}
func confirmCheckpointProcesses(processes []checkpointProcess) error {
	if len(processes) != 2 {
		return errors.New("checkpoint process absence lacks recorded identities")
	}
	for _, p := range processes {
		start, err := processStart(p.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if start == p.Start {
			return fmt.Errorf("checkpoint process %d remains", p.PID)
		}
	}
	return nil
}
func (m *Manager) stopCheckpointProcesses(ctx context.Context, id string, s *checkpointState) error {
	if confirmCheckpointProcesses(s.Processes) == nil {
		return nil
	}
	control, err := m.vmControl()
	if err != nil {
		return err
	}
	_, stopErr := control.StopVM(m.ctx(ctx), &proto.StopVMRequest{VMID: id, TimeoutSeconds: 2})
	// The native paused-VM stop returns Internal after force termination. Accept
	// it only once both recorded process identities are positively absent.
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		err = confirmCheckpointProcesses(s.Processes)
		if err == nil {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return errors.Join(stopErr, err, context.Cause(waitCtx))
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (m *Manager) stopCheckpoint(ctx context.Context, id string) error {
	f, err := m.checkpointLock(ctx, id, true, false)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlockCheckpoint(f)
	s, err := m.loadCheckpoint(id)
	if os.IsNotExist(err) {
		return m.removeCheckpointNetwork(ctx, id)
	}
	if err != nil {
		return err
	}
	if s.Phase != "suspended" && s.Phase != "removed" {
		if len(s.Processes) != 2 {
			s.Processes, err = m.checkpointProcesses(ctx, id)
			if err != nil {
				control, controlErr := m.vmControl()
				if controlErr != nil {
					return errors.Join(err, controlErr)
				}
				if _, stopErr := control.StopVM(m.ctx(ctx), &proto.StopVMRequest{VMID: id, TimeoutSeconds: 2}); stopErr != nil {
					return errors.Join(err, stopErr)
				}
				s.Phase = "removed"
				if err = m.saveCheckpoint(id, s); err != nil {
					return err
				}
			}
			if s.Phase != "removed" {
				s.Phase = "stopping"
			}
			if err = m.saveCheckpoint(id, s); err != nil {
				return err
			}
		}
		// Pausing avoids guest shutdown writing to a retained memory checkpoint.
		if control, e := m.vmControl(); e == nil {
			if snap, ok := control.(snapshotControl); ok {
				_, _ = snap.PauseVM(ctx, &proto.PauseVMRequest{VMID: id})
			}
		}
		if s.Phase != "removed" {
			if err = m.stopCheckpointProcesses(ctx, id, s); err != nil {
				return err
			}
		}
	} else if s.Phase == "suspended" {
		if err = confirmCheckpointProcesses(s.Processes); err != nil {
			return err
		}
	}
	if err = m.removeCheckpointNetwork(ctx, id); err != nil {
		return err
	}
	s.Phase = "removed"
	if err = m.saveCheckpoint(id, s); err != nil {
		return err
	}
	// Keep the lock inode until metadata/disk deletion finishes; state blocks new calls.
	entries, err := os.ReadDir(m.runtimeDir(id))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-") {
			if err = os.RemoveAll(filepath.Join(m.runtimeDir(id), e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
