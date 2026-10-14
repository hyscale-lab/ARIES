package apptainer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// fakedShell starts a fakeroot daemon for the whole instance, outside the
// command's own fakeroot library, and prints its key and PID. It prints nothing
// when Apptainer injected no fakeroot, as without a fakeroot command on PATH.
const fakedShell = `faked=/.singularity.d/libs/faked; [ -x "$faked" ] || exit 0; unset LD_PRELOAD FAKEROOTKEY; exec "$faked"`

// startFaked gives the instance one fakeroot daemon and puts its key in the
// runtime's environment, which every command reads. Otherwise each apptainer
// exec has its own daemon, which ends with the command. A harness that saves a
// command's environment and restores it in the next, as Hermes does, would then
// send to a finished daemon's queue: nothing reads it, so it fills, and the
// command blocks in msgsnd. Stopping the instance ends the daemon.
func (manager *Manager) startFaked(ctx context.Context, id string) error {
	output, err := manager.run(ctx, nil, append(execPrefix(id, "/"), "/bin/sh", "-c", fakedShell)...)
	if err != nil {
		return fmt.Errorf("start the instance's fakeroot daemon: %w", err)
	}
	reply := strings.TrimSpace(string(output))
	if reply == "" {
		return nil
	}
	key, pid, found := strings.Cut(reply, ":")
	if _, err := strconv.ParseUint(key, 10, 64); err != nil || !found {
		return fmt.Errorf("unexpected reply from the instance's fakeroot daemon: %q", reply)
	}
	if _, err := strconv.ParseUint(pid, 10, 64); err != nil {
		return fmt.Errorf("unexpected reply from the instance's fakeroot daemon: %q", reply)
	}
	path := filepath.Join(manager.sharePath(id), environmentFile)
	environment, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read deployment environment: %w", err)
	}
	if err := writePrivate(path, append(environment, environmentScript(nil, map[string]string{"FAKEROOTKEY": key})...)); err != nil {
		return fmt.Errorf("write deployment environment: %w", err)
	}
	return nil
}
