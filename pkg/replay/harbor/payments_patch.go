package harbor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hyscale-lab/aries/pkg/replay"
)

const VerifyPatch = "VerifyPatch"

const (
	PaymentsRejectedPatchTrial       = "1532d3f9-a790-4d9b-b849-42c48290acd7"
	PaymentsRejectedNestedPatchTrial = "3bcd8259-9e48-4322-8c01-e925035743eb"
)

const duplicatePaymentsTarget = "apply_patch verification failed: invalid patch: multiple operations target /app/src/worker/worker.py"

var paymentsVerification = map[string]struct {
	step              int
	callID, scriptSHA string
	interval          time.Duration
}{
	PaymentsRejectedPatchTrial:       {15, "call_bBxouF9cvJzYX2astbTzh4a8", "55bea47fd50dcc6b31ee3b70447f1a6d045cfdddd2361d2f688d99985bc3c59e", 91 * time.Millisecond},
	PaymentsRejectedNestedPatchTrial: {17, "call_fR8cS37ZToC1PweMVGuyo98k", "b805caacd566fd20ce7f994cafd7e94cf460fc846474be41562dd1fe453ce464", 73 * time.Millisecond},
}

func paymentsPatchVerification(trial string, c indexedCall, cwd string) (replay.Call, time.Duration, bool) {
	pin, ok := paymentsVerification[trial]
	if !ok || cwd != "/app" || c.step != pin.step || c.call.ID != pin.callID ||
		c.call.Name != "exec_command" || c.output != duplicatePaymentsTarget {
		return replay.Call{}, 0, false
	}
	var args struct{ Cmd string }
	if json.Unmarshal(c.call.Args, &args) != nil || digest([]byte(args.Cmd)) != pin.scriptSHA {
		return replay.Call{}, 0, false
	}
	return replay.Call{Operation: VerifyPatch, Workdir: cwd, ToolCallID: c.call.ID,
		Script: args.Cmd, ResponseStdout: []byte(c.output)}, pin.interval, true
}

func (h Hooks) preserveVerificationInterval(ctx context.Context, step int, interval time.Duration, begin, end int64) (int64, error) {
	if h.clock != nil {
		target := h.clock.lastDispatch + int64(interval)
		if h.clock.runnable(end) < target {
			return h.waitRunnable(ctx, step, target)
		}
		return end, nil
	}
	remaining := time.Duration(begin-end) + interval
	if remaining <= 0 {
		return end, nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return end, ctx.Err()
	case <-timer.C:
		return Clock()
	}
}
