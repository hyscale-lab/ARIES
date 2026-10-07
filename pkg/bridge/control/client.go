package control

import (
	"context"
	"errors"
	"maps"
	"net"
	"strings"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// NewClient permits cleartext only over local sockets or loopback (including a
// private authenticated infrastructure tunnel). Remote callers must supply mTLS.
// Automatic RPC retries are disabled; callers reconcile using the original ID.
func NewClient(address, instance, token string, transport credentials.TransportCredentials) (*grpc.ClientConn, v1.BridgeControlClient, error) {
	if !identifier.MatchString(instance) || len(token) < 32 {
		return nil, nil, errors.New("invalid control client identity")
	}
	if transport == nil {
		host, _, err := net.SplitHostPort(address)
		if !strings.HasPrefix(address, "unix://") && (err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
			return nil, nil, errors.New("remote bridge control requires mutually authenticated TLS")
		}
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(transport), grpc.WithDisableRetry(), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token, "x-aries-instance", instance)
		return invoker(ctx, method, req, reply, cc, opts...)
	}))
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
