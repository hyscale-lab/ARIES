package openclaw

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultDockerBinary = "docker"
	maxDockerOutput     = 16 << 20
)

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

type commandRunner interface {
	Run(context.Context, []byte, ...string) (commandResult, error)
}

type execRunner struct {
	binary string
}

func (runner execRunner) Run(ctx context.Context, stdin []byte, args ...string) (commandResult, error) {
	command := exec.CommandContext(ctx, runner.binary, args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr limitedBuffer
	stdout.limit = maxDockerOutput
	stderr.limit = maxDockerOutput
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	if stdout.exceeded || stderr.exceeded {
		return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}, errors.New("Docker command output exceeded its bound")
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}, err
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	if len(content) > buffer.limit-buffer.Len() {
		content = content[:max(0, buffer.limit-buffer.Len())]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}

func runDockerChecked(ctx context.Context, cli commandRunner, secret []byte, stdin []byte, args ...string) (commandResult, error) {
	result, err := cli.Run(ctx, stdin, args...)
	return checkedDockerResult(ctx, result, err, secret, args...)
}

func checkedDockerResult(ctx context.Context, result commandResult, commandErr error, secret []byte, args ...string) (commandResult, error) {
	if commandErr == nil && result.exitCode == 0 {
		return result, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return result, contextErr
	}
	detail := strings.TrimSpace(string(redactBytes(result.stderr, secret)))
	if len(detail) > 512 {
		detail = detail[:512] + "..."
	}
	if detail == "" {
		detail = "no stderr"
	}
	return result, fmt.Errorf("docker %s: exit %d: %s", strings.Join(safeCommandSummary(args), " "), result.exitCode, detail)
}

func safeCommandSummary(args []string) []string {
	if len(args) > 12 {
		args = args[:12]
	}
	summary := make([]string, len(args))
	for index, value := range args {
		if len(value) > 120 {
			value = value[:120] + "..."
		}
		summary[index] = strconv.QuoteToASCII(value)
	}
	return summary
}

type containerInspection struct {
	ID     string `json:"Id"`
	Config struct {
		Image       string            `json:"Image"`
		User        string            `json:"User"`
		Env         []string          `json:"Env"`
		Cmd         []string          `json:"Cmd"`
		Entrypoint  []string          `json:"Entrypoint"`
		Labels      map[string]string `json:"Labels"`
		Healthcheck *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

type volumeInspection struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

func inspectContainer(ctx context.Context, cli commandRunner, secret []byte, name string) (containerInspection, error) {
	inspection, exists, err := inspectContainerMaybe(ctx, cli, secret, name)
	if err != nil {
		return containerInspection{}, err
	}
	if !exists {
		return containerInspection{}, fmt.Errorf("OpenClaw container %q is absent", name)
	}
	return inspection, nil
}

func inspectContainerMaybe(ctx context.Context, cli commandRunner, secret []byte, name string) (containerInspection, bool, error) {
	result, err := cli.Run(ctx, nil, "container", "inspect", name)
	if err != nil || result.exitCode != 0 {
		if contextErr := ctx.Err(); contextErr != nil {
			return containerInspection{}, false, contextErr
		}
		message := strings.ToLower(string(redactBytes(result.stderr, secret)))
		if strings.Contains(message, "no such container") || strings.Contains(message, "not found") {
			return containerInspection{}, false, nil
		}
		return containerInspection{}, false, fmt.Errorf("inspect OpenClaw container: %s", strings.TrimSpace(message))
	}
	var inspections []containerInspection
	if err := json.Unmarshal(result.stdout, &inspections); err != nil {
		return containerInspection{}, false, fmt.Errorf("decode OpenClaw container inspection: %w", err)
	}
	if len(inspections) != 1 {
		return containerInspection{}, false, fmt.Errorf("OpenClaw container inspection returned %d records", len(inspections))
	}
	return inspections[0], true, nil
}

func inspectVolume(ctx context.Context, cli commandRunner, secret []byte, name string) (volumeInspection, bool, error) {
	result, err := cli.Run(ctx, nil, "volume", "inspect", name)
	if err != nil || result.exitCode != 0 {
		if contextErr := ctx.Err(); contextErr != nil {
			return volumeInspection{}, false, contextErr
		}
		message := strings.ToLower(string(redactBytes(result.stderr, secret)))
		if strings.Contains(message, "no such volume") || strings.Contains(message, "not found") {
			return volumeInspection{}, false, nil
		}
		return volumeInspection{}, false, fmt.Errorf("inspect OpenClaw volume: %s", strings.TrimSpace(message))
	}
	var inspections []volumeInspection
	if err := json.Unmarshal(result.stdout, &inspections); err != nil {
		return volumeInspection{}, false, fmt.Errorf("decode OpenClaw volume inspection: %w", err)
	}
	if len(inspections) != 1 {
		return volumeInspection{}, false, fmt.Errorf("OpenClaw volume inspection returned %d records", len(inspections))
	}
	return inspections[0], true, nil
}

func containerAbsent(ctx context.Context, cli commandRunner, secret []byte, name string) (bool, error) {
	result, err := cli.Run(ctx, nil, "container", "inspect", name)
	if err == nil && result.exitCode == 0 {
		return false, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	message := strings.ToLower(string(redactBytes(result.stderr, secret)))
	if strings.Contains(message, "no such container") || strings.Contains(message, "not found") {
		return true, nil
	}
	return false, fmt.Errorf("inspect removed OpenClaw container: %s", strings.TrimSpace(message))
}

func volumeAbsent(ctx context.Context, cli commandRunner, secret []byte, name string) (bool, error) {
	result, err := cli.Run(ctx, nil, "volume", "inspect", name)
	if err == nil && result.exitCode == 0 {
		return false, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	message := strings.ToLower(string(redactBytes(result.stderr, secret)))
	if strings.Contains(message, "no such volume") || strings.Contains(message, "not found") {
		return true, nil
	}
	return false, fmt.Errorf("inspect removed OpenClaw volume: %s", strings.TrimSpace(message))
}

func containerProcesses(ctx context.Context, cli commandRunner, secret []byte, name string) ([]string, error) {
	result, err := cli.Run(ctx, nil, "container", "top", name, "-eo", "pid,args")
	if err != nil || result.exitCode != 0 {
		message := strings.ToLower(string(redactBytes(result.stderr, secret)))
		if strings.Contains(message, "not running") || strings.Contains(message, "no such container") {
			return nil, nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("inspect OpenClaw processes: %s", strings.TrimSpace(message))
	}
	var processes []string
	for index, line := range strings.Split(strings.TrimSpace(string(result.stdout)), "\n") {
		if index == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
			return nil, errors.New("Docker top returned a non-numeric process ID")
		}
		processes = append(processes, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
	}
	return processes, nil
}

func copyContainerFile(ctx context.Context, cli commandRunner, secret []byte, container, path string) ([]byte, bool, error) {
	result, err := cli.Run(ctx, nil, "container", "cp", container+":"+path, "-")
	if err != nil || result.exitCode != 0 {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, false, contextErr
		}
		message := strings.ToLower(string(redactBytes(result.stderr, secret)))
		if strings.Contains(message, "could not find") || strings.Contains(message, "no such file") || strings.Contains(message, "not found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("copy OpenClaw container file %q: %s", path, strings.TrimSpace(message))
	}
	reader := tar.NewReader(bytes.NewReader(result.stdout))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("read Docker copy archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if header.Size < 0 || header.Size > maxDockerOutput {
			return nil, false, errors.New("Docker copy file exceeds its bound")
		}
		content, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(content)) != header.Size {
			return nil, false, errors.New("Docker copy file is truncated")
		}
		return content, true, nil
	}
	return nil, false, errors.New("Docker copy archive contains no regular file")
}

func copyContainerArchive(ctx context.Context, cli commandRunner, secret []byte, container, path string) ([]byte, bool, error) {
	result, err := cli.Run(ctx, nil, "container", "cp", container+":"+path, "-")
	if err != nil || result.exitCode != 0 {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, false, contextErr
		}
		message := strings.ToLower(string(redactBytes(result.stderr, secret)))
		if strings.Contains(message, "could not find") || strings.Contains(message, "no such file") || strings.Contains(message, "not found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("copy OpenClaw state archive: %s", strings.TrimSpace(message))
	}
	return result.stdout, true, nil
}

func cleanArchivePath(value string) (string, bool) {
	cleaned := filepath.ToSlash(filepath.Clean(value))
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}
