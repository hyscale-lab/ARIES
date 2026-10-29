package vhive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/libcni"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-go-sdk/cni/vmconf"
	"golang.org/x/sys/unix"
)

// The logical sandbox owns CNI resources independently of any one VMM. Native
// CreateVM receives only the resulting static interface, so StopVM leaves the
// namespace, tap and IP allocation available for snapshot restore.
type checkpointNetwork struct {
	ContainerID    string                             `json:"container_id"`
	NetNS          string                             `json:"netns"`
	NamespaceOwned bool                               `json:"namespace_owned"`
	AddAttempted   bool                               `json:"add_attempted"`
	BinPaths       []string                           `json:"bin_paths"`
	CacheDir       string                             `json:"cache_dir"`
	Config         json.RawMessage                    `json:"config"`
	Interface      *proto.FirecrackerNetworkInterface `json:"interface,omitempty"`
}

func (m *Manager) networkStatePath(id string) string {
	return filepath.Join(m.runtimeDir(id), "network.json")
}

func saveCheckpointNetwork(path string, state *checkpointNetwork, exclusive bool) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if exclusive {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			return errors.Join(err, os.Remove(path))
		}
		return nil
	}
	// Replace atomically: cleanup must retain the complete CNI ownership record.
	file, err := os.CreateTemp(filepath.Dir(path), ".network-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (m *Manager) loadCheckpointNetwork(id string) (*checkpointNetwork, error) {
	data, err := os.ReadFile(m.networkStatePath(id))
	if err != nil {
		return nil, err
	}
	var state checkpointNetwork
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("read checkpoint network ownership: %w", err)
	}
	if state.ContainerID != id || state.NetNS != filepath.Join(m.runtimeDir(id), "netns") {
		return nil, errors.New("checkpoint network ownership does not match sandbox")
	}
	return &state, nil
}

func (m *Manager) checkpointNetworkConfig(id string) (*proto.FirecrackerNetworkInterface, string, error) {
	state, err := m.loadCheckpointNetwork(id)
	if err != nil {
		return nil, "", err
	}
	if state.Interface == nil || state.Interface.StaticConfig == nil || state.Interface.CNIConfig != nil || !state.NamespaceOwned {
		return nil, "", errors.New("checkpoint network allocation is incomplete")
	}
	return state.Interface, state.NetNS, nil
}

func staticCheckpointInterface(config *vmconf.StaticNetworkConf) (*proto.FirecrackerNetworkInterface, error) {
	if config == nil || config.TapName == "" || config.VMMacAddr == "" || config.VMIPConfig == nil {
		return nil, errors.New("CNI did not return a complete VM network configuration")
	}
	nameservers := append([]string(nil), config.VMNameservers...)
	if len(nameservers) > 2 {
		nameservers = nameservers[:2]
	}
	gateway := ""
	if len(config.VMIPConfig.Gateway) != 0 {
		gateway = config.VMIPConfig.Gateway.String()
	}
	return &proto.FirecrackerNetworkInterface{StaticConfig: &proto.StaticNetworkConfiguration{
		MacAddress: config.VMMacAddr, HostDevName: config.TapName,
		IPConfig: &proto.IPConfiguration{PrimaryAddr: config.VMIPConfig.Address.String(), GatewayAddr: gateway, Nameservers: nameservers},
	}}, nil
}

func (m *Manager) prepareCheckpointNetwork(ctx context.Context, id string) (*proto.FirecrackerNetworkInterface, string, error) {
	if _, err := os.Stat(m.networkStatePath(id)); err == nil {
		return m.checkpointNetworkConfig(id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(m.runtimeDir(id), 0700); err != nil {
		return nil, "", err
	}
	state := &checkpointNetwork{ContainerID: id, NetNS: filepath.Join(m.runtimeDir(id), "netns"), BinPaths: m.options.CNIBinPaths, CacheDir: m.options.CNICacheDir}
	if _, err := os.Lstat(state.NetNS); err == nil {
		return nil, "", errors.New("refusing preexisting checkpoint network namespace")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	if len(state.BinPaths) == 0 {
		state.BinPaths = []string{"/opt/cni/bin"}
	}
	if state.CacheDir == "" {
		state.CacheDir = filepath.Join("/var/lib/cni", id)
	}
	confDir := m.options.CNIConfDir
	if confDir == "" {
		confDir = "/etc/cni/conf.d"
	}
	config, err := libcni.LoadConfList(confDir, m.options.CNINetwork)
	if err != nil {
		return nil, "", fmt.Errorf("load checkpoint CNI config: %w", err)
	}
	state.Config = config.Bytes
	if err := saveCheckpointNetwork(m.networkStatePath(id), state, true); err != nil {
		return nil, "", err
	}
	// O_EXCL establishes ownership before any mount, and never adopts a foreign path.
	file, err := os.OpenFile(state.NetNS, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, "", err
	}
	if err := file.Close(); err != nil {
		return nil, "", errors.Join(err, os.Remove(state.NetNS))
	}
	state.NamespaceOwned = true
	if err := saveCheckpointNetwork(m.networkStatePath(id), state, false); err != nil {
		return nil, "", errors.Join(err, os.Remove(state.NetNS))
	}
	if err := mountCheckpointNetNS(state.NetNS); err != nil {
		return nil, "", err
	}
	state.AddAttempted = true
	if err := saveCheckpointNetwork(m.networkStatePath(id), state, false); err != nil {
		return nil, "", err
	}
	cni := libcni.NewCNIConfigWithCacheDir(state.BinPaths, state.CacheDir, nil)
	result, err := cni.AddNetworkList(ctx, config, state.runtimeConfig())
	if err != nil {
		return nil, "", fmt.Errorf("allocate checkpoint CNI network: %w", err)
	}
	parsed, err := vmconf.StaticNetworkConfFrom(result, id)
	if err != nil {
		return nil, "", err
	}
	if parsed.NetNSPath != state.NetNS {
		return nil, "", errors.New("CNI returned a different checkpoint network namespace")
	}
	state.Interface, err = staticCheckpointInterface(parsed)
	if err != nil {
		return nil, "", err
	}
	if err := saveCheckpointNetwork(m.networkStatePath(id), state, false); err != nil {
		return nil, "", err
	}
	return state.Interface, state.NetNS, nil
}

func (state *checkpointNetwork) runtimeConfig() *libcni.RuntimeConf {
	return &libcni.RuntimeConf{ContainerID: state.ContainerID, NetNS: state.NetNS, IfName: "veth0"}
}

func mountCheckpointNetNS(path string) error {
	done := make(chan error, 1)
	go func() {
		// Discard this OS thread on return; never release its namespace to Go's pool.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			done <- fmt.Errorf("create checkpoint network namespace: %w", err)
			return
		}
		done <- unix.Mount("/proc/thread-self/ns/net", path, "none", unix.MS_BIND, "none")
	}()
	return <-done
}

func (m *Manager) removeCheckpointNetwork(ctx context.Context, id string) error {
	state, err := m.loadCheckpointNetwork(id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.AddAttempted {
		config, err := libcni.ConfListFromBytes(state.Config)
		if err != nil {
			return err
		}
		cni := libcni.NewCNIConfigWithCacheDir(state.BinPaths, state.CacheDir, nil)
		if err := cni.DelNetworkList(ctx, config, state.runtimeConfig()); err != nil {
			return fmt.Errorf("remove checkpoint CNI network: %w", err)
		}
	}
	if state.NamespaceOwned {
		if err := unix.Unmount(state.NetNS, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unmount checkpoint network namespace: %w", err)
		}
		if err := os.Remove(state.NetNS); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(m.networkStatePath(id))
}
