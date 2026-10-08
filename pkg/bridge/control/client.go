package control

import (
	"maps"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NewClient connects to the configured bridge using plaintext gRPC.
// Automatic RPC retries are disabled; callers reconcile using the original ID.
func NewClient(address string) (*grpc.ClientConn, v1.BridgeControlClient, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		return nil, nil, err
	}
	return conn, v1.NewBridgeControlClient(conn), nil
}
func TargetToProto(t core.BridgeTarget) *v1.Target {
	return &v1.Target{Version: uint32(t.Version), RunId: t.RunID, TaskId: t.TaskID, OccurrenceId: t.OccurrenceID, Backend: t.Backend, RuntimeId: t.RuntimeID, RuntimeName: t.RuntimeName, ExpectedLabels: maps.Clone(t.ExpectedLabels), Workdir: t.Workdir, ExecUser: t.ExecUser, MaxInputBytes: t.MaxInputBytes, MaxOutputBytes: t.MaxOutputBytes}
}
func TargetFromProto(t *v1.Target) core.BridgeTarget {
	if t == nil {
		return core.BridgeTarget{}
	}
	return core.BridgeTarget{Version: int(t.Version), RunID: t.RunId, TaskID: t.TaskId, OccurrenceID: t.OccurrenceId, Backend: t.Backend, RuntimeID: t.RuntimeId, RuntimeName: t.RuntimeName, ExpectedLabels: maps.Clone(t.ExpectedLabels), Workdir: t.Workdir, ExecUser: t.ExecUser, MaxInputBytes: t.MaxInputBytes, MaxOutputBytes: t.MaxOutputBytes}
}
