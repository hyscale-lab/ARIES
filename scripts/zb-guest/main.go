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

	"github.com/mdlayher/vsock"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use snapshot, serve, request, counters, or calibrate")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var pfnStart, pfnCount uint64
	var socket, dir string
	var hostSocket bool
	var workloadPID int
	var once bool
	switch command {
	case "counters":
		fs.BoolVar(&once, "once", false, "emit one counter sample and exit")
		fs.IntVar(&workloadPID, "workload-pid", 0, "workload container init PID in the guest")
	case "snapshot", "serve":
		fs.Uint64Var(&pfnStart, "pfn-start", 0, "first guest PFN to inspect")
		fs.Uint64Var(&pfnCount, "pfn-count", 0, "number of guest PFNs to inspect, zero disables")
		if command == "serve" {
			fs.StringVar(&socket, "socket", "", "Unix socket path in a private directory")
			fs.BoolVar(&hostSocket, "host-vsock", false, "serve the fixed snapshot request to the host CID on port 4099")
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
	case "counters":
		return counters(workloadPID, once, encoder)
	case "snapshot":
		return encoder.Encode(snapshot(pfnStart, pfnCount))
	case "serve":
		if hostSocket {
			if socket != "" {
				return errors.New("choose a single telemetry transport")
			}
			listener, err := vsock.Listen(4099, nil)
			if err != nil {
				return err
			}
			return serveListener(listener, pfnStart, pfnCount, os.Getenv("ZB_TORCH_WHEEL_DONTNEED") == "1")
		}
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
	return serveListener(listener, start, count, false)
}

func serveListener(listener net.Listener, start, count uint64, wheelControl bool) error {
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
		if peer, ok := conn.RemoteAddr().(*vsock.Addr); ok && peer.ContextID != vsock.Host {
			conn.Close()
			continue
		}
		if handleTelemetry(conn, start, count, wheelControl) {
			wheelControl = false
		}
	}
}

func handleSnapshot(conn net.Conn, start, count uint64) {
	handleTelemetry(conn, start, count, false)
}

func handleTelemetry(conn net.Conn, start, count uint64, wheelControl bool) (used bool) {
	defer conn.Close()
	if conn.SetDeadline(time.Now().Add(10*time.Second)) != nil {
		return false
	}
	var request [len(snapshotRequest)]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil {
		return false
	}
	switch string(request[:]) {
	case snapshotRequest:
		_ = json.NewEncoder(conn).Encode(snapshot(start, count))
	case "evictwhl\n":
		if wheelControl && count == 0 && os.Getenv("ZB_TORCH_FILE_CACHE") == "1" {
			_ = json.NewEncoder(conn).Encode(releaseTorchWheels())
			return true
		}
	}
	return false
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
