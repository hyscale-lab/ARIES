package vhive

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/ttrpc"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/protobuf/encoding/protowire"
	pb "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// The older vHive client bindings already cover the common VM controls. These
// are the two added wire fields in damon-virtio-pmem's proto/firecracker.proto.
const nativeCheckpointDirectoryField = 22
const freePageReportingField = 4

func nativeReportingBalloon() *proto.FirecrackerBalloonDevice {
	balloon := &proto.FirecrackerBalloonDevice{StatsPollingIntervals: 1}
	field := protowire.AppendTag(nil, freePageReportingField, protowire.VarintType)
	balloon.ProtoReflect().SetUnknown(protowire.AppendVarint(field, 1))
	return balloon
}

var checkpointVMDescriptor = sync.OnceValues(func() (protoreflect.MessageDescriptor, error) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:   pb.String("aries_native_checkpoint.proto"),
		Syntax: pb.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: pb.String("CheckpointVMRequest"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: pb.String("VMID"), Number: pb.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: pb.String("Directory"), Number: pb.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
			},
		}},
	}, nil)
	if err != nil {
		return nil, err
	}
	return file.Messages().ByName("CheckpointVMRequest"), nil
})

func nativeRestoreRequest(id string, state *checkpointState) *proto.CreateVMRequest {
	request := &proto.CreateVMRequest{
		VMID:              id,
		TimeoutSeconds:    state.Request.TimeoutSeconds,
		NetNS:             state.Request.NetNS,
		NetworkInterfaces: state.Request.NetworkInterfaces,
	}
	field := protowire.AppendTag(nil, nativeCheckpointDirectoryField, protowire.BytesType)
	request.ProtoReflect().SetUnknown(protowire.AppendString(field, state.NativeDirectory))
	return request
}

func (m *Manager) suspendNativeCheckpoint(ctx context.Context, id string, state *checkpointState) error {
	dir, err := os.MkdirTemp(m.runtimeDir(id), "snapshot-")
	if err != nil {
		return err
	}
	state.NativeDirectory = filepath.Join(dir, "native")
	state.Memory = filepath.Join(state.NativeDirectory, "memory")
	state.Snapshot = filepath.Join(state.NativeDirectory, "state")
	state.Phase = "capturing"
	if err := m.saveCheckpoint(id, state); err != nil {
		return err
	}
	endCapture := core.TimeCommandStage(ctx, "checkpoint.native_capture")
	err = m.captureNativeCheckpoint(ctx, id, state.NativeDirectory)
	endCapture()
	if err != nil {
		return fmt.Errorf("capture native vHive checkpoint: %w", err)
	}
	return m.finishCheckpointSuspend(ctx, id, state, dir)
}

func (m *Manager) captureNativeCheckpoint(ctx context.Context, id, directory string) error {
	desc, err := checkpointVMDescriptor()
	if err != nil {
		return err
	}
	request := dynamicpb.NewMessage(desc)
	request.Set(desc.Fields().ByNumber(1), protoreflect.ValueOfString(id))
	request.Set(desc.Fields().ByNumber(2), protoreflect.ValueOfString(directory))
	path, closePath, err := shortUnixSocketPath(m.options.Socket + ".ttrpc")
	if err != nil {
		return err
	}
	defer closePath()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	return client.Call(ctx, "Firecracker", "CheckpointVM", request, &emptypb.Empty{})
}

// ParkNativeCheckpoint stops a restored, isolated VM while retaining its
// original native artifact and guest task identity for a second Resume.
func (m *Manager) ParkNativeCheckpoint(ctx context.Context, id string) error {
	if !m.options.NativeCheckpoint || m.options.IdleMode != "checkpoint" {
		return fmt.Errorf("parking requires native checkpoint mode")
	}
	lock, err := m.checkpointLock(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer unlockCheckpoint(lock)
	ctx = m.ctx(ctx)
	state, err := m.loadCheckpoint(id)
	if err != nil {
		return err
	}
	if state.Phase != "running" || state.NativeDirectory == "" || !state.TaskRunning ||
		state.Request.ContainerCount > 1 || state.Request.NetNS != "" || len(state.Request.NetworkInterfaces) != 0 {
		return fmt.Errorf("cannot park native checkpoint in phase %q, running task %t, container count %d, network interfaces %d", state.Phase, state.TaskRunning, state.Request.ContainerCount, len(state.Request.NetworkInterfaces))
	}
	processes, err := m.checkpointProcesses(ctx, id)
	if err != nil {
		return err
	}
	state.Processes = processes
	state.Phase = "stopping"
	if err := m.saveCheckpoint(id, state); err != nil {
		return err
	}
	if err := m.stopCheckpointProcesses(ctx, id, state); err != nil {
		return err
	}
	state.Phase = "suspended"
	return m.saveCheckpoint(id, state)
}
