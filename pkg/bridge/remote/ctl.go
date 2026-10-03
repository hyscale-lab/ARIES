package remote

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Call sends one request to the daemon's socket and returns its response.
func Call(socket string, request Request, timeout time.Duration) (Response, error) {
	connection, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return Response{}, fmt.Errorf("connect to bridge daemon: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(timeout))
	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return Response{}, fmt.Errorf("send to bridge daemon: %w", err)
	}
	line, err := bufio.NewReader(&limitedReader{reader: connection, remaining: maxMessageBytes}).ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("read from bridge daemon: %w", err)
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		return Response{}, fmt.Errorf("parse bridge daemon response: %w", err)
	}
	return response, nil
}

// RunCtl is `aries-bridge ctl`: it relays one JSON request from stdin to the
// daemon and prints the JSON response. It exits non-zero only when the daemon
// could not be reached, so a refused request still reports the instance and
// grant state to the runner.
func RunCtl(socket string, stdin io.Reader, stdout io.Writer, timeout time.Duration) error {
	var request Request
	decoder := json.NewDecoder(io.LimitReader(stdin, maxMessageBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	response, err := Call(socket, request, timeout)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(response)
}

// RunCollect is `aries-bridge collect GRANT_ID`: it asks the daemon for a
// revoked grant's logs and writes them to stdout as a tar stream, named by
// base name only. The daemon, not this process, decides the grant is revoked.
func RunCollect(socket, grantID string, stdout io.Writer, timeout time.Duration) error {
	response, err := Call(socket, Request{Op: OpCollect, GrantID: grantID}, timeout)
	if err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	archive := tar.NewWriter(stdout)
	for _, path := range response.Files {
		if err := appendLog(archive, path); err != nil {
			return err
		}
	}
	return archive.Close()
}

// appendLog writes one log file without following symbolic links, sized from
// the open descriptor so the header and the content cannot disagree.
func appendLog(archive *tar.Writer, path string) error {
	name := filepath.Base(path)
	if err := validateLogFile(name); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	if err := archive.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := io.CopyN(archive, file, info.Size()); err != nil {
		return fmt.Errorf("copy %s: %w", name, err)
	}
	return nil
}
