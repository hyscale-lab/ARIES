package hermes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	native "github.com/hyscale-lab/aries/pkg/harness/hermes/gateway"
)

const gatewayPort = 8642
const gatewayKeyPath = stagedRoot + "/gateway.key"

type gatewayExecution struct {
	client  *native.Client
	mu      sync.Mutex
	runID   string
	cancel  context.CancelFunc
	stopped bool
}

func newGatewayExecution(address string, token []byte) (*gatewayExecution, error) {
	client, err := native.New(address, string(token))
	if err != nil {
		return nil, err
	}
	return &gatewayExecution{client: client}, nil
}

// Cancellation is advisory. Deployment removal remains the termination proof.
func (execution *gatewayExecution) cancelRun(ctx context.Context) {
	execution.mu.Lock()
	execution.stopped = true
	if execution.cancel != nil {
		execution.cancel()
	}
	id := execution.runID
	execution.mu.Unlock()
	if id != "" {
		cancelCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = execution.client.Stop(cancelCtx, id)
		cancel()
	}
}

// runOutcome records the native service state and the submission-to-observation
// boundary. It makes no claim about CLI exit codes or upstream compute time.
type runOutcome struct {
	Status     string `json:"status"`
	EndReason  string `json:"end_reason"`
	RunID      string `json:"run_id,omitempty"`
	SessionID  string `json:"session_id"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
	DurationMS int64  `json:"duration_ms"`
}

func (manager *Manager) runGateway(ctx context.Context, active *session, instruction string) (string, runOutcome, error) {
	started := time.Now()
	outcome := runOutcome{Status: string(core.StatusFailed), EndReason: "request_error", SessionID: "aries-" + active.AttemptID, StartedAt: started.UTC().Format(time.RFC3339Nano)}
	execution := active.gateway
	runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
	defer cancel()
	execution.mu.Lock()
	if execution.stopped {
		execution.mu.Unlock()
		outcome.EndReason = "stopped"
		outcome.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return "", outcome, errors.New("Hermes Gateway is stopped")
	}
	execution.cancel = cancel
	execution.mu.Unlock()
	id, err := execution.client.Submit(runCtx, instruction, outcome.SessionID)
	var result native.Run
	if err == nil {
		outcome.RunID = id
		execution.mu.Lock()
		execution.runID = id
		execution.mu.Unlock()
		result, err = execution.client.Wait(runCtx, id, 100*time.Millisecond)
		if err == nil {
			outcome.EndReason = result.Status
			if result.SessionID != "" && result.SessionID != outcome.SessionID {
				outcome.EndReason = "session_mismatch"
				err = errors.New("Hermes Gateway returned a different task session")
			} else if result.Status == "completed" {
				outcome.Status = string(core.StatusSucceeded)
			} else {
				if result.Status == "cancelled" || result.Status == "interrupted" {
					outcome.Status = string(core.StatusCanceled)
				}
				err = fmt.Errorf("Hermes Gateway run %s: %s", result.Status, result.Error)
				if outcome.Status == string(core.StatusCanceled) {
					err = errors.Join(context.Canceled, err)
				}
			}
		}
	}
	if result.Status == "" && errors.Is(err, context.DeadlineExceeded) {
		outcome.Status, outcome.EndReason = string(core.StatusCanceled), "deadline_exceeded"
	}
	if result.Status == "" && errors.Is(err, context.Canceled) {
		outcome.Status, outcome.EndReason = string(core.StatusCanceled), "canceled"
	}
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		execution.cancelRun(cleanupCtx)
		cleanupCancel()
	}
	ended := time.Now()
	outcome.EndedAt = ended.UTC().Format(time.RFC3339Nano)
	outcome.DurationMS = ended.Sub(started).Milliseconds()
	return result.Output, outcome, err
}
