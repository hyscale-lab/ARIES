package vhive

import (
	"bytes"
	"testing"

	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"google.golang.org/protobuf/encoding/protowire"
	pb "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestNativeCheckpointWireCompatibility(t *testing.T) {
	const id, directory = "same-id", "/checkpoint/native"
	desc, err := checkpointVMDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	capture := dynamicpb.NewMessage(desc)
	capture.Set(desc.Fields().ByNumber(1), protoreflect.ValueOfString(id))
	capture.Set(desc.Fields().ByNumber(2), protoreflect.ValueOfString(directory))
	actual, err := pb.MarshalOptions{Deterministic: true}.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), id)
	want = protowire.AppendString(protowire.AppendTag(want, 2, protowire.BytesType), directory)
	if !bytes.Equal(actual, want) {
		t.Fatalf("unexpected native capture encoding %x", actual)
	}
	restore := nativeRestoreRequest(id, &checkpointState{
		Request:         &proto.CreateVMRequest{VMID: id, NetNS: "/test/netns", ContainerCount: 1},
		NativeDirectory: directory,
	})
	if restore.VMID != id || restore.NetNS != "/test/netns" || restore.ContainerCount != 0 || restore.LoadSnapshot {
		t.Fatalf("native restore should only supply restore overrides, got %v", restore)
	}
	want = protowire.AppendString(protowire.AppendTag(nil, 22, protowire.BytesType), directory)
	if !bytes.Equal(restore.ProtoReflect().GetUnknown(), want) {
		t.Fatalf("unexpected native restore directory encoding %x", restore.ProtoReflect().GetUnknown())
	}
}

func TestNativeReportingBalloonWireCompatibility(t *testing.T) {
	balloon := nativeReportingBalloon()
	if balloon.AmountMib != 0 || balloon.DeflateOnOom || balloon.StatsPollingIntervals != 1 {
		t.Fatalf("unexpected reporting balloon settings: %v", balloon)
	}
	want := protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 1)
	if !bytes.Equal(balloon.ProtoReflect().GetUnknown(), want) {
		t.Fatalf("unexpected free-page reporting encoding %x", balloon.ProtoReflect().GetUnknown())
	}
}
