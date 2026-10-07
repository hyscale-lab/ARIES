package codexssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

func validWorkdir(value string) bool {
	return strings.HasPrefix(value, "/") && path.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

// The root-level name is created exclusively before any agent access. Keeping
// it outside /tmp avoids following a benchmark-controlled ancestor symlink.
func (session *bridgeSession) stageExecutor(ctx context.Context, codexPath, supervisorPath string) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	session.stageDir = "/.aries-codex-" + hex.EncodeToString(random[:])
	result, err := session.sandbox.Exec(ctx, core.Command{
		Path: "/bin/mkdir", Args: []string{"-m", "0755", "--", session.stageDir}, User: "0:0",
	})
	if err != nil || result.ExitCode != 0 {
		return errors.Join(err, fmt.Errorf("create exclusive Codex executor directory: status %d", result.ExitCode))
	}
	session.stageOwned = true
	if err := session.sandbox.Upload(ctx, codexPath, session.stageDir+"/codex"); err != nil {
		return fmt.Errorf("stage native Codex executor: %w", err)
	}
	if err := session.sandbox.Upload(ctx, supervisorPath, session.stageDir+"/supervisor"); err != nil {
		return fmt.Errorf("stage Codex descendant supervisor: %w", err)
	}
	session.supervisorStaged = true
	result, err = session.sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", `mkdir -m 0777 -- "$1/home" && chmod 0777 -- "$1/home" && chmod 0555 -- "$1" "$1/codex" "$1/supervisor"`, "aries-codex-stage", session.stageDir}, User: "0:0",
	})
	if err != nil || result.ExitCode != 0 {
		return errors.Join(err, fmt.Errorf("prepare Codex executor directory: status %d", result.ExitCode))
	}
	return nil
}

func (session *bridgeSession) removeExecutor(ctx context.Context) error {
	if !session.stageOwned {
		return nil
	}
	if session.executorAttempted {
		return errors.New("Codex supervisor did not confirm staged executor removal")
	}
	if session.supervisorStaged {
		result, err := session.sandbox.ExecSupervisedStream(ctx, core.Command{
			Path: session.stageDir + "/supervisor", Args: []string{"--cleanup-stage", session.stageDir}, User: "0:0",
		}, nil, io.Discard, io.Discard)
		if err != nil || result.ExitCode != 0 {
			return errors.Join(err, fmt.Errorf("confirm unused Codex executor removal: status %d", result.ExitCode))
		}
		session.stageOwned = false
		return nil
	}
	// This rollback is only reachable before staging the supervisor and before
	// publishing any agent access. No task command runs after native execution.
	result, err := session.sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", `rm -rf -- "$1" && test ! -e "$1" && test ! -L "$1"`, "aries-codex-cleanup", session.stageDir}, User: "0:0",
	})
	if err != nil || result.ExitCode != 0 {
		return errors.Join(err, fmt.Errorf("confirm Codex executor directory removal: status %d", result.ExitCode))
	}
	session.stageOwned = false
	return nil
}

func stageExecutable(source, destination string) (returnErr error) {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return errors.New("Codex SSH client source is not a regular executable")
	}
	dst, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, dst.Close()) }()
	_, err = io.Copy(dst, src)
	return errors.Join(err, dst.Sync())
}
