package sandbox

import (
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
)

// PrepareCommand preserves sandbox validation and defaults for borrowed execution.
func PrepareCommand(command core.Command, workdir, user string) (core.Command, error) {
	if err := validateCommand(command); err != nil {
		return core.Command{}, err
	}
	if command.Dir == "" {
		command.Dir = workdir
	}
	if command.User == "" {
		command.User = user
	}
	if err := validateCommand(command); err != nil {
		return core.Command{}, err
	}
	return command, nil
}

// ExportBridgeTarget exports execution metadata, never runtime ownership.
func (s *Sandbox) ExportBridgeTarget() (core.BridgeTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.containerOwned || s.stopping || s.stopped {
		return core.BridgeTarget{}, errors.New("sandbox is not available for bridge assignment")
	}
	backend, ok := s.deployment.(interface{ BridgeBackend() string })
	if !ok {
		return core.BridgeTarget{}, errors.New("sandbox deployment does not support borrowed execution")
	}
	descriptor := core.BridgeTarget{RunID: s.runID, TaskID: s.taskID, SandboxID: s.containerName, Backend: backend.BridgeBackend(), RuntimeID: s.containerID, Workdir: s.workdir, ExecUser: s.execUser}
	return descriptor, nil
}
