// zb-guest samples the running guest kernel without changing its memory policy.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use snapshot, serve, request, or calibrate")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var pfnStart, pfnCount uint64
	var socket, dir string
	switch command {
	case "snapshot", "serve":
		fs.Uint64Var(&pfnStart, "pfn-start", 0, "first guest PFN to inspect")
		fs.Uint64Var(&pfnCount, "pfn-count", 0, "number of guest PFNs to inspect, zero disables")
		if command == "serve" {
			fs.StringVar(&socket, "socket", "", "Unix socket path in a private directory")
		}
	case "request":
		fs.StringVar(&socket, "socket", "", "running local guest telemetry socket")
	case "calibrate":
		fs.StringVar(&dir, "directory", "", "directory on the filesystem under test")
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || pfnCount > maxPFNs || pfnStart > maxPFNOffset-pfnCount {
		return errors.New("unexpected arguments or excessive PFN range")
	}
	encoder := json.NewEncoder(out)
	switch command {
	case "snapshot":
		return encoder.Encode(snapshot(pfnStart, pfnCount))
	case "serve":
		return serve(socket, pfnStart, pfnCount)
	case "request":
		return request(socket, out)
	case "calibrate":
		if dir == "" {
			return errors.New("calibrate requires --directory")
		}
		return calibrate(dir, encoder)
	}
	return errors.New("unsupported command")
}

const snapshotRequest = "snapshot\n"
const maxResponseBytes = 64 << 20

func serve(path string, start, count uint64) error {
	if !filepath.IsAbs(path) {
		return errors.New("serve requires an absolute socket path")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return errors.New("socket directory must be private")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer listener.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			listener.Close()
		case <-done:
		}
	}()
	for {
		conn, err := listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		handleSnapshot(conn, start, count)
	}
}

func handleSnapshot(conn net.Conn, start, count uint64) {
	defer conn.Close()
	if conn.SetDeadline(time.Now().Add(10*time.Second)) != nil {
		return
	}
	var request [len(snapshotRequest)]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil || string(request[:]) != snapshotRequest {
		return
	}
	_ = json.NewEncoder(conn).Encode(snapshot(start, count))
}

func request(path string, out io.Writer) error {
	if !filepath.IsAbs(path) {
		return errors.New("request requires an absolute socket path")
	}
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, snapshotRequest); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(conn, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxResponseBytes || !json.Valid(body) {
		return errors.New("invalid guest telemetry reply")
	}
	_, err = out.Write(body)
	return err
}
