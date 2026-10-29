package vhive

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-go-sdk/cni/vmconf"
)

func TestStaticCheckpointInterfacePreservesGuestNetwork(t *testing.T) {
	config := &vmconf.StaticNetworkConf{
		TapName: "tap0", VMMacAddr: "06:00:00:01:02:03",
		VMIPConfig:    &current.IPConfig{Address: net.IPNet{IP: net.ParseIP("192.0.2.7"), Mask: net.CIDRMask(24, 32)}, Gateway: net.ParseIP("192.0.2.1")},
		VMNameservers: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"},
	}
	iface, err := staticCheckpointInterface(config)
	if err != nil {
		t.Fatal(err)
	}
	expected := &proto.FirecrackerNetworkInterface{StaticConfig: &proto.StaticNetworkConfiguration{HostDevName: "tap0", MacAddress: config.VMMacAddr, IPConfig: &proto.IPConfiguration{PrimaryAddr: "192.0.2.7/24", GatewayAddr: "192.0.2.1", Nameservers: []string{"192.0.2.1", "192.0.2.2"}}}}
	if !reflect.DeepEqual(iface, expected) {
		t.Fatalf("static network mismatch: %+v", iface)
	}
	iface.StaticConfig.IPConfig.Nameservers[0] = "changed"
	if config.VMNameservers[0] != "192.0.2.1" {
		t.Fatal("static config aliases CNI result")
	}
	for _, invalid := range []*vmconf.StaticNetworkConf{nil, {}, {TapName: "tap0", VMMacAddr: "06:00:00:01:02:03"}} {
		if _, err := staticCheckpointInterface(invalid); err == nil {
			t.Fatal("accepted incomplete CNI configuration")
		}
	}
}

func networkTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{options: Options{CheckpointDir: t.TempDir(), Namespace: "test"}}
}

func TestCheckpointNetworkPersistenceAndOwnership(t *testing.T) {
	m := networkTestManager(t)
	id := "sandbox"
	if err := os.MkdirAll(m.runtimeDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	state := &checkpointNetwork{ContainerID: id, NetNS: filepath.Join(m.runtimeDir(id), "netns"), NamespaceOwned: true, Interface: &proto.FirecrackerNetworkInterface{StaticConfig: &proto.StaticNetworkConfiguration{HostDevName: "tap0", MacAddress: "06:00:00:01:02:03"}}}
	path := m.networkStatePath(id)
	if err := saveCheckpointNetwork(path, state, true); err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpointNetwork(path, state, true); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive ownership: %v", err)
	}
	iface, namespace, err := m.checkpointNetworkConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	if namespace != state.NetNS || !reflect.DeepEqual(iface, state.Interface) {
		t.Fatal("checkpoint network changed after reload")
	}
	// Existing ready allocations are read without touching CNI or host namespaces.
	iface, namespace, err = m.prepareCheckpointNetwork(context.Background(), id)
	if err != nil || namespace != state.NetNS || !reflect.DeepEqual(iface, state.Interface) {
		t.Fatalf("reuse allocation: %v", err)
	}
	state.ContainerID = "foreign"
	if err := saveCheckpointNetwork(path, state, false); err != nil {
		t.Fatal(err)
	}
	if err := m.removeCheckpointNetwork(context.Background(), id); err == nil {
		t.Fatal("accepted mismatched ownership")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed foreign ownership record")
	}
}

func TestCheckpointNetworkRejectsForeignNamespace(t *testing.T) {
	m := networkTestManager(t)
	id := "sandbox"
	if err := os.MkdirAll(m.runtimeDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.runtimeDir(id), "netns")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.prepareCheckpointNetwork(context.Background(), id); err == nil {
		t.Fatal("adopted foreign namespace")
	}
	if err := m.removeCheckpointNetwork(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "foreign" {
		t.Fatalf("foreign namespace modified: %v", err)
	}
}

func TestCheckpointNetworkIncompleteAllocationIsNotRecreated(t *testing.T) {
	m := networkTestManager(t)
	id := "sandbox"
	if err := os.MkdirAll(m.runtimeDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	state := &checkpointNetwork{ContainerID: id, NetNS: filepath.Join(m.runtimeDir(id), "netns")}
	if err := saveCheckpointNetwork(m.networkStatePath(id), state, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.prepareCheckpointNetwork(context.Background(), id); err == nil {
		t.Fatal("accepted incomplete allocation")
	}
	// The initial ownership record never claimed the namespace, so cleanup must
	// preserve even a path that appears after the failed setup.
	if err := os.WriteFile(state.NetNS, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.removeCheckpointNetwork(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state.NetNS); err != nil {
		t.Fatal("cleanup removed an unowned namespace")
	}
}
