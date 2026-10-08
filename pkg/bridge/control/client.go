package control

import (
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NewClient uses plaintext gRPC. Callers reconcile uncertain registration using
// the sandbox ID; automatic RPC retries remain disabled.
func NewClient(address string) (*grpc.ClientConn, v1.BridgeControlClient, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry())
	if err != nil {
		return nil, nil, err
	}
	return conn, v1.NewBridgeControlClient(conn), nil
}
func Registration(d core.BridgeTarget) *v1.RegisterSandboxRequest {
	return &v1.RegisterSandboxRequest{SandboxId: d.SandboxID, Target: &v1.Target{RuntimeId: d.RuntimeID, Workdir: d.Workdir, ExecUser: d.ExecUser}, Metadata: &v1.Metadata{TaskId: d.TaskID}}
}
func TargetFromRegistration(r *v1.RegisterSandboxRequest, runID, backend string) core.BridgeTarget {
	return core.BridgeTarget{RunID: runID, TaskID: r.GetMetadata().GetTaskId(), SandboxID: r.GetSandboxId(), Backend: backend, RuntimeID: r.GetTarget().GetRuntimeId(), Workdir: r.GetTarget().GetWorkdir(), ExecUser: r.GetTarget().GetExecUser()}
}
