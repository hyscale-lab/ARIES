package vhive

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"strings"
	"testing"
)

const sampleMapping = "1000-3000 rw-p 00000000 00:00 0\nSize: 8 kB\nRss: 4 kB\nPss: 3 kB\nPrivate_Clean: 0 kB\nPrivate_Dirty: 2 kB\nShared_Clean: 2 kB\nShared_Dirty: 0 kB\nAnonymous: 4 kB\nAnonHugePages: 0 kB\nTHPeligible: 0\nVmFlags: rd wr mr mw me ac\n"

func TestMemoryMappingAccountingRemainsSeparate(t *testing.T) {
	got, e := parseSmaps(sampleMapping)
	if e != nil {
		t.Fatal(e)
	}
	m := got[0]
	if m.Start != 4096 || m.End != 12288 || m.Bytes["Rss"] != 4096 || m.Bytes["Pss"] != 3072 || m.Bytes["Private_Dirty"] != 2048 {
		t.Fatalf("wrong mapping units: %+v", m)
	}
	if len(m.Flags) != 6 {
		t.Fatalf("flags: %v", m.Flags)
	}
	// No sum of RSS/PSS/private or identification as guest RAM is manufactured.
	if _, ok := m.Bytes["WorkingSet"]; ok {
		t.Fatal("invented working set")
	}
}
func TestMemoryMappingRejectsIncompleteAndInconsistentEvidence(t *testing.T) {
	for _, data := range []string{"", "Rss: 0 kB\n", strings.Replace(sampleMapping, "Rss: 4 kB\n", "", 1), strings.Replace(sampleMapping, "Size: 8 kB", "Size: 4 kB", 1), sampleMapping + "Rss: 4 kB\n", sampleMapping + sampleMapping, strings.Replace(sampleMapping, "Rss: 4 kB", "Rss: 18446744073709551615 kB", 1)} {
		if _, e := parseSmaps(data); e == nil {
			t.Fatalf("accepted malformed evidence %q", data)
		}
	}
}

type memoryTestVM struct {
	vmClient
	socket string
}

func (v *memoryTestVM) GetVMInfo(_ context.Context, r *proto.GetVMInfoRequest) (*proto.GetVMInfoResponse, error) {
	return &proto.GetVMInfoResponse{VMID: r.VMID, SocketPath: v.socket}, nil
}

func TestObservedMachineConfigurationBindsSocketPeer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/machine-config" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vcpu_count":2,"mem_size_mib":4096}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{options: Options{Namespace: "aries"}, vm: &memoryTestVM{socket: path}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity := checkpointProcess{PID: os.Getpid(), Start: start}
	got, err := m.observedMachineConfiguration(ctx, "owned", identity)
	if err != nil || got.VCPUCount != 2 || got.MemoryMiB != 4096 {
		t.Fatalf("capacity: %+v, error: %v", got, err)
	}
	for _, wrong := range []checkpointProcess{{PID: identity.PID + 1, Start: start}, {PID: identity.PID, Start: start + "0"}} {
		if _, err = m.observedMachineConfiguration(ctx, "owned", wrong); err == nil {
			t.Fatalf("accepted wrong socket identity: %+v", wrong)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("sent HTTP requests before rejecting peer: %d", got)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = m.observedMachineConfiguration(cancelled, "owned", identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestMemoryCompletionChecksBothProcessIdentities(t *testing.T) {
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	live := checkpointProcess{PID: os.Getpid(), Start: start}
	if err = confirmMemoryProcesses([]checkpointProcess{live, live}); err != nil {
		t.Fatal(err)
	}
	for changed := range 2 {
		processes := []checkpointProcess{live, live}
		processes[changed].Start += "0"
		if err = confirmMemoryProcesses(processes); err == nil {
			t.Fatalf("accepted changed process at index %d", changed)
		}
	}
	if err = confirmMemoryProcesses([]checkpointProcess{live, {PID: 0}}); err == nil {
		t.Fatal("accepted invisible shim identity")
	}
}
