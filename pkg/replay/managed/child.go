package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/replay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// RunChild reaches the private bridge before it accepts a staged replay plan.
func RunChild(ctx context.Context, configPath string) error {
	folder := filepath.Dir(configPath)
	data, err := privateFile(configPath, maxStatusBytes)
	if err != nil {
		return fmt.Errorf("read private replay launch configuration: %w", err)
	}
	var launch childLaunch
	if err := json.Unmarshal(data, &launch); err != nil {
		return err
	}
	if launch.CallTimeout <= 0 {
		return errors.New("replay caller requires a positive call timeout")
	}
	if host, _, err := net.SplitHostPort(launch.Endpoint); err != nil || host == "" {
		return errors.New("invalid private replay bridge endpoint")
	}
	connection, err := grpc.NewClient(launch.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDisableRetry(), grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(bridgegrpc.MaxMessageBytes)))
	if err != nil {
		return err
	}
	defer connection.Close()
	readyCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	connection.Connect()
	for {
		state := connection.GetState()
		if state == connectivity.Ready {
			break
		}
		if !connection.WaitForStateChange(readyCtx, state) {
			return finishChild(ctx, folder, readyCtx.Err())
		}
	}
	if err := writePrivate(filepath.Join(folder, readyFile), nil); err != nil {
		return err
	}
	var plan replay.Plan
	for {
		_, err = privateFile(filepath.Join(folder, planReadyFile), 0)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return finishChild(ctx, folder, err)
		}
		if err := poll(ctx); err != nil {
			return nil
		}
	}
	data, err = privateFile(filepath.Join(folder, planFile), maxPlanBytes)
	if err != nil {
		return finishChild(ctx, folder, err)
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		return finishChild(ctx, folder, err)
	}
	wait, err := executionTimeout(plan, launch.CallTimeout)
	if err != nil {
		return finishChild(ctx, folder, err)
	}
	callCtx, done := context.WithTimeout(ctx, wait)
	err = bridgegrpc.ReplayCalls(callCtx, launch.CallTimeout, sandboxv1.NewSandboxClient(connection), plan)
	done()
	return finishChild(ctx, folder, err)
}

func executionTimeout(plan replay.Plan, callTimeout time.Duration) (time.Duration, error) {
	const maxDuration = time.Duration(1<<63 - 1)
	if plan.FinalWait < 0 || plan.FinalWait > maxDuration-callTimeout {
		return 0, errors.New("invalid replay schedule")
	}
	last := time.Duration(0)
	for _, call := range plan.Calls {
		if call.StartOffset < 0 || call.StartOffset > maxDuration-callTimeout-plan.FinalWait {
			return 0, errors.New("invalid replay schedule")
		}
		last = max(last, call.StartOffset)
	}
	return last + callTimeout + plan.FinalWait, nil
}

func finishChild(ctx context.Context, folder string, resultErr error) error {
	if err := writeResult(folder, resultErr); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func writeResult(folder string, resultErr error) error {
	result := childResult{Done: true}
	if resultErr != nil {
		result.Error = resultErr.Error()
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(folder, resultFile), data)
}

func writePrivate(name string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(name), ".aries-replay-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), name)
}

func privateFile(name string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxBytes {
		return nil, errors.New("invalid private replay caller file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, errors.New("private replay caller file changed during read")
	}
	return data, nil
}
