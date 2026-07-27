package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

const runResultName = "run-result.json"

func newRunID(now time.Time, experimentName string) string {
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + experimentName
}

func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	return logger
}

func attachRunLog(logger *logrus.Logger, outputRoot string) (func() error, error) {
	path := filepath.Join(outputRoot, "aries.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create run log: %w", err)
	}
	original := logger.Out
	logger.SetOutput(io.MultiWriter(original, file))
	return func() error {
		logger.SetOutput(original)
		return file.Close()
	}, nil
}

func createRunOutputRoot(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.Mkdir(path, 0o700)
}

func executeAndRecord(
	ctx context.Context,
	execute func(context.Context) (core.RunResult, error),
	outputRoot string,
	stdout io.Writer,
) error {
	result, runErr := execute(ctx)
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errors.Join(runErr, fmt.Errorf("encode run result: %w", err))
	}
	content = append(content, '\n')

	persistErr := persistRunResult(filepath.Join(outputRoot, runResultName), content)
	_, stdoutErr := io.Copy(stdout, bytes.NewReader(content))
	if persistErr != nil {
		persistErr = fmt.Errorf("persist run result: %w", persistErr)
	}
	if stdoutErr != nil {
		stdoutErr = fmt.Errorf("write run result to stdout: %w", stdoutErr)
	}
	return errors.Join(runErr, persistErr, stdoutErr)
}

func persistRunResult(path string, content []byte) error {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("run output root is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("run output root permissions are %04o; want 0700", info.Mode().Perm())
	}

	temporary, err := os.CreateTemp(directory, ".result-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	temporaryPath = ""
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}
