package vhive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"golang.org/x/sys/unix"
)

// MemoryObservation is detailed Linux mapping evidence, not ResourceReading's
// aggregate gauge. It intentionally provides no total-host-footprint denominator:
// controller, host page-cache and kernel allocations are not attributed here.
type MemoryObservation struct {
	SchemaVersion        int                          `json:"schema_version"`
	RuntimeID            string                       `json:"runtime_id"`
	StartedAt            time.Time                    `json:"started_at"`
	FinishedAt           time.Time                    `json:"finished_at"`
	Processes            []ProcessMemory              `json:"processes"`
	Coverage             string                       `json:"coverage"`
	Excluded             []string                     `json:"excluded"`
	MachineConfiguration ObservedMachineConfiguration `json:"machine_configuration"`
}

// ObservedMachineConfiguration is read from Firecracker, not inferred from limits.
type ObservedMachineConfiguration struct {
	VCPUCount int `json:"vcpu_count"`
	MemoryMiB int `json:"mem_size_mib"`
}
type ProcessMemory struct {
	Role       string          `json:"role"`
	PID        int             `json:"pid"`
	StartTicks string          `json:"start_ticks"`
	Mappings   []MemoryMapping `json:"mappings"`
}
type MemoryMapping struct {
	Start       uint64 `json:"start"`
	End         uint64 `json:"end"`
	Permissions string `json:"permissions"`
	Offset      uint64 `json:"offset"`
	Device      string `json:"device"`
	Inode       uint64 `json:"inode"`
	// Mapping name is diagnostic; neither names nor anonymous-map size establish
	// a guest physical address. A separately verified KVM slot map is required.
	Name  string            `json:"name,omitempty"`
	Bytes map[string]uint64 `json:"bytes"`
	Flags []string          `json:"flags,omitempty"`
}

// ObserveMemory resolves the VMM using the native VM control socket's peer
// credentials, includes its parent shim, and rejects process identity changes.
// ENOENT remains an error: confirmed teardown is owned by Stop, not this reader.
func (m *Manager) ObserveMemory(ctx context.Context, id string) (MemoryObservation, error) {
	obs := MemoryObservation{SchemaVersion: 1, RuntimeID: id, StartedAt: time.Now().UTC(),
		Coverage: "VMM and parent shim mappings only; not total attributable host memory",
		Excluded: []string{"host kernel allocations", "host disk/page cache outside these mappings", "shared containerd controller", "bridge", "harness", "inference"}}
	identities, err := m.checkpointProcesses(ctx, id)
	if err != nil {
		return obs, err
	}
	for i, p := range identities {
		if err = ctx.Err(); err != nil {
			return obs, err
		}
		before, e := processStart(p.PID)
		if e != nil {
			return obs, e
		}
		if before != p.Start {
			return obs, errors.New("memory process identity changed before scan")
		}
		data, e := os.ReadFile(fmt.Sprintf("/proc/%d/smaps", p.PID))
		if e != nil {
			return obs, e
		}
		mappings, e := parseSmaps(string(data))
		if e != nil {
			return obs, e
		}
		after, e := processStart(p.PID)
		if e != nil {
			return obs, e
		}
		if after != p.Start {
			return obs, errors.New("memory process identity changed during scan")
		}
		role := "vmm"
		if i == 1 {
			role = "shim"
		}
		obs.Processes = append(obs.Processes, ProcessMemory{Role: role, PID: p.PID, StartTicks: p.Start, Mappings: mappings})
	}
	if err = ctx.Err(); err != nil {
		return obs, err
	}
	machine, err := m.observedMachineConfiguration(ctx, id, identities[0])
	if err != nil {
		return obs, err
	}
	if err = confirmMemoryProcesses(identities); err != nil {
		return obs, err
	}
	if err = ctx.Err(); err != nil {
		return obs, err
	}
	obs.MachineConfiguration = machine
	obs.FinishedAt = time.Now().UTC()
	return obs, nil
}

func parseSmaps(data string) ([]MemoryMapping, error) {
	var mappings []MemoryMapping
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		address := strings.Split(fields[0], "-")
		if len(address) == 2 && !strings.HasSuffix(fields[0], ":") {
			if len(fields) < 5 {
				return nil, errors.New("truncated smaps mapping")
			}
			start, e := strconv.ParseUint(address[0], 16, 64)
			if e != nil {
				return nil, e
			}
			end, e := strconv.ParseUint(address[1], 16, 64)
			if e != nil {
				return nil, e
			}
			if end <= start || len(mappings) > 0 && start < mappings[len(mappings)-1].End {
				return nil, errors.New("unordered or overlapping smaps mapping")
			}
			offset, e := strconv.ParseUint(fields[2], 16, 64)
			if e != nil {
				return nil, e
			}
			inode, e := strconv.ParseUint(fields[4], 10, 64)
			if e != nil {
				return nil, e
			}
			name := ""
			if len(fields) > 5 {
				name = strings.Join(fields[5:], " ")
			}
			mappings = append(mappings, MemoryMapping{Start: start, End: end, Permissions: fields[1], Offset: offset, Device: fields[3], Inode: inode, Name: name, Bytes: map[string]uint64{}})
			continue
		}
		if len(mappings) == 0 {
			return nil, errors.New("smaps field without mapping")
		}
		current := &mappings[len(mappings)-1]
		key := strings.TrimSuffix(fields[0], ":")
		if key == "VmFlags" {
			current.Flags = append([]string(nil), fields[1:]...)
			continue
		}
		if len(fields) == 3 && fields[2] == "kB" {
			value, e := strconv.ParseUint(fields[1], 10, 64)
			if e != nil {
				return nil, e
			}
			if value > ^uint64(0)/1024 {
				return nil, errors.New("smaps byte count overflow")
			}
			if _, ok := current.Bytes[key]; ok {
				return nil, fmt.Errorf("duplicate smaps field %s", key)
			}
			current.Bytes[key] = value * 1024
		}
	}
	if len(mappings) == 0 {
		return nil, errors.New("empty smaps")
	}
	for _, m := range mappings {
		for _, key := range []string{"Size", "Rss", "Pss", "Private_Clean", "Private_Dirty", "Shared_Clean", "Shared_Dirty"} {
			if _, ok := m.Bytes[key]; !ok {
				return nil, fmt.Errorf("missing smaps field %s", key)
			}
		}
		if m.Bytes["Size"] != m.End-m.Start {
			return nil, errors.New("smaps mapping size differs")
		}
	}
	return mappings, nil
}

func (m *Manager) observedMachineConfiguration(ctx context.Context, id string, vmm checkpointProcess) (ObservedMachineConfiguration, error) {
	var result ObservedMachineConfiguration
	control, err := m.vmControl()
	if err != nil {
		return result, err
	}
	info, err := control.GetVMInfo(m.ctx(ctx), &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return result, err
	}
	path, closePath, err := shortUnixSocketPath(strings.TrimPrefix(info.SocketPath, "unix://"))
	if err != nil {
		return result, err
	}
	defer closePath()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialMemorySocket(ctx, path, vmm)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://firecracker/machine-config", nil)
	if err != nil {
		return result, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("machine-config HTTP %d", response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return result, err
	}
	if result.VCPUCount < 1 || result.MemoryMiB < 128 {
		return result, errors.New("missing or invalid observed machine capacity")
	}
	return result, nil
}

// The capacity reply and the mappings must belong to the same VMM generation.
func dialMemorySocket(ctx context.Context, path string, expected checkpointProcess) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	reject := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return reject(err)
	}
	var cred *unix.Ucred
	var credErr error
	if err = raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return reject(err)
	}
	if credErr != nil {
		return reject(credErr)
	}
	if int(cred.Pid) != expected.PID {
		return reject(errors.New("memory machine-config socket VMM identity differs"))
	}
	if err = confirmMemoryProcesses([]checkpointProcess{expected}); err != nil {
		return reject(err)
	}
	return conn, nil
}

func confirmMemoryProcesses(processes []checkpointProcess) error {
	for _, p := range processes {
		start, err := processStart(p.PID)
		if err != nil {
			return err
		}
		if start != p.Start {
			return errors.New("memory process identity changed during observation")
		}
	}
	return nil
}
