package vhive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func (m *Manager) UploadArchive(ctx context.Context, id, destination string, archive io.Reader) error {
	if !path.IsAbs(destination) || path.Clean(destination) != destination || strings.ContainsRune(destination, 0) {
		return errors.New("vHive archive destination must be a clean absolute path")
	}
	var stderr bytes.Buffer
	result, err := m.ExecStream(ctx, id, core.Command{Path: "tar", Args: []string{"-xpf", "-", "-C", destination}}, archive, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("extract vHive archive (exit %d): %s", result.ExitCode, stderr.String())
	}
	return nil
}

func (m *Manager) DownloadArchive(ctx context.Context, id, source string) (io.ReadCloser, deployment.FileInfo, error) {
	if !path.IsAbs(source) || path.Clean(source) != source || source == "/" || strings.ContainsRune(source, 0) {
		return nil, deployment.FileInfo{}, errors.New("vHive archive source must be a clean absolute non-root path")
	}
	stat, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", `if [ ! -e "$1" ] && [ ! -L "$1" ]; then exit 44; fi; exec stat -c '%s %f' -- "$1"`, "aries-stat", source}})
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if stat.ExitCode == 44 {
		return nil, deployment.FileInfo{}, fmt.Errorf("vHive archive source: %w", runner.ErrNotFound)
	}
	if stat.ExitCode != 0 {
		return nil, deployment.FileInfo{}, fmt.Errorf("stat vHive archive source: %s", stat.Stderr)
	}
	info, err := parseStat(stat.Stdout)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	r, w := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		// File transfers stream independently of the per-tool output limit.
		result, err := m.ExecStream(streamCtx, id, core.Command{Path: "tar", Args: []string{"-cpf", "-", "-C", path.Dir(source), "--", path.Base(source)}, OutputLimitBytes: 1 << 30}, nil, w, &stderr)
		if err == nil && result.ExitCode != 0 {
			err = fmt.Errorf("create vHive archive: %s", stderr.String())
		}
		_ = w.CloseWithError(err)
	}()
	return &archiveStream{PipeReader: r, cancel: cancel}, info, nil
}

type archiveStream struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (s *archiveStream) Close() error { s.cancel(); return s.PipeReader.Close() }

func parseStat(s string) (deployment.FileInfo, error) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return deployment.FileInfo{}, errors.New("unexpected vHive stat output")
	}
	size, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || size < 0 {
		return deployment.FileInfo{}, errors.New("invalid vHive file size")
	}
	raw, err := strconv.ParseUint(f[1], 16, 32)
	if err != nil {
		return deployment.FileInfo{}, err
	}
	mode := fs.FileMode(raw & 0o777)
	switch raw & 0o170000 {
	case 0o100000:
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	default:
		mode |= fs.ModeIrregular
	}
	return deployment.FileInfo{Size: size, Mode: mode}, nil
}
