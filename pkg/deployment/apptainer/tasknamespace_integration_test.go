//go:build apptainer

package apptainer

// These tests start real task namespaces, with slirp4netns, nsenter and cat
// on PATH, and need unprivileged user namespaces. The test binary itself
// serves the ports, since the test images have no server.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// TestMain lets the test binary serve a marker from inside a task namespace,
// where nsenter starts it.
func TestMain(m *testing.M) {
	if marker := os.Getenv("ARIES_TEST_MARKER"); marker != "" {
		os.Exit(serveMarker(marker, os.Getenv("ARIES_TEST_PORT")))
	}
	os.Exit(m.Run())
}

// serveMarker answers each connection to port with marker until its input
// closes.
func serveMarker(marker, port string) int {
	listener, err := net.Listen("tcp4", "0.0.0.0:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go answer(listener, marker)
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func answer(listener net.Listener, marker string) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		_, _ = io.WriteString(connection, marker+"\n")
		_ = connection.Close()
	}
}

// startMarker serves marker on port inside the task namespace of holder, and
// returns the function that ends it.
func startMarker(t *testing.T, holder int, marker string, port int) func() {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("nsenter", "--user", "--net", "--preserve-credentials", "--target", strconv.Itoa(holder), self)
	command.Env = append(os.Environ(), "ARIES_TEST_MARKER="+marker, "ARIES_TEST_PORT="+strconv.Itoa(port))
	command.Stdin, command.Stderr = input, os.Stderr
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = release.Close()
		t.Fatal(err)
	}
	_ = input.Close()
	return func() {
		if release.Close() == nil {
			_ = command.Wait()
		}
	}
}

// readMarker reads the first line served at address, retrying while the
// server starts.
func readMarker(t *testing.T, address string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			line, readErr := bufio.NewReader(connection).ReadString('\n')
			_ = connection.Close()
			if readErr == nil {
				return strings.TrimSpace(line)
			}
			err = readErr
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing served at %s: %v", address, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRealTaskNamespacesServeOnePortApart(t *testing.T) {
	// A short state directory: t.TempDir's, named after the test, can make a
	// task's slirp4netns socket path too long.
	manager := newRealManagerIn(t, shortTempDir(t))
	ctx := context.Background()

	// The test stands in for the run namespace; a listener on its loopback
	// plays the bridge.
	bridge, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	go answer(bridge, "bridge")
	bridgePort := bridge.Addr().(*net.TCPAddr).Port

	// The test's own network namespace stands in for the run's, so the shared
	// port is one that nothing on this host holds.
	free, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	type task struct {
		namespace *runningTaskNamespace
		runtime   string
		marker    func()
	}
	tasks := make([]*task, 2)
	for index := range tasks {
		loopback, err := taskLoopback(uint32(4000 + index))
		if err != nil {
			t.Fatal(err)
		}
		handle, err := startTaskNamespace(ctx, taskNamespaceRequest{
			stateDir: manager.options.StateDir, runtime: fmt.Sprintf("aries-test-%d", index), loopback: loopback,
			labels: map[string]string{"aries.managed": "true", "aries.run": "run-1", "aries.task": fmt.Sprintf("task-%d", index)},
		})
		if handle != nil {
			// Stopped at the end even when the start failed and left it.
			t.Cleanup(func() {
				if err := handle.Stop(context.Background()); err != nil {
					t.Errorf("cleanup Stop of %s: %v", handle.Name(), err)
				}
			})
		}
		if err != nil {
			t.Fatalf("startTaskNamespace: %v", err)
		}
		namespace := handle.(*runningTaskNamespace)
		if err := namespace.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		request := realRequest(t)
		request.Placement.AttachmentID = taskNamespacePrefix + namespace.Name()
		request.ServicePort = port
		id, err := manager.Create(ctx, request)
		// By the requested name: a Create that cannot confirm its instance
		// returns no identity, and Stop of the name still finds and ends it.
		t.Cleanup(func() {
			if err := manager.Stop(context.Background(), request.Name); err != nil {
				t.Errorf("cleanup Stop: %v", err)
			}
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := manager.Start(ctx, id); err != nil {
			t.Fatalf("Start: %v", err)
		}
		tasks[index] = &task{namespace: namespace, runtime: id}
	}
	if tasks[0].namespace.record.Network == tasks[1].namespace.record.Network {
		t.Fatal("two tasks share a network namespace")
	}

	// Each instance is in its task's network namespace and reaches the run's
	// loopback through the task's gateway.
	for index, task := range tasks {
		script := "readlink /proc/self/ns/net; exec 3<>/dev/tcp/" + taskGateway + "/" + strconv.Itoa(bridgePort) + " && head -n 1 <&3"
		result, err := manager.Exec(ctx, task.runtime, core.Command{Path: "/bin/bash", Args: []string{"-c", script}})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("task %d: Exec = %+v, %v", index, result, err)
		}
		if want := task.namespace.record.Network + "\nbridge\n"; result.Stdout != want {
			t.Fatalf("task %d: %q, want %q", index, result.Stdout, want)
		}
	}

	// Both tasks serve the same port at once; each is reached at its own
	// address.
	for index, task := range tasks {
		task.marker = startMarker(t, task.namespace.record.HolderPID, fmt.Sprintf("task-%d", index), port)
		t.Cleanup(task.marker)
	}
	for index, task := range tasks {
		address, err := manager.Address(ctx, task.runtime, port)
		if err != nil {
			t.Fatalf("Address: %v", err)
		}
		if got := readMarker(t, address); got != fmt.Sprintf("task-%d", index) {
			t.Fatalf("%s answered %q for task %d", address, got, index)
		}
		if again, err := manager.Address(ctx, task.runtime, port); err != nil || again != address {
			t.Fatalf("a second Address = %q, %v", again, err)
		}
	}
	// The gateway leads to the run namespace's 127.0.0.1, not to the port the
	// other task forwards from its own loopback address.
	probe := "exec 3<>/dev/tcp/" + taskGateway + "/" + strconv.Itoa(port)
	if result, err := manager.Exec(ctx, tasks[0].runtime, core.Command{Path: "/bin/bash", Args: []string{"-c", probe}}); err != nil || result.ExitCode == 0 {
		t.Fatalf("a task reached port %d through its gateway: %+v, %v", port, result, err)
	}
	// Nor does the other task's forward address, which Runner reached above:
	// inside a task, every 127.0.0.0/8 address is the task's own loopback, so
	// task 0 is answered by its own server.
	peer, err := manager.Address(ctx, tasks[1].runtime, port)
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(peer)
	probe = "exec 3<>/dev/tcp/" + host + "/" + strconv.Itoa(port) + " && head -n 1 <&3"
	if result, err := manager.Exec(ctx, tasks[0].runtime, core.Command{Path: "/bin/bash", Args: []string{"-c", probe}}); err != nil || result.Stdout != "task-0\n" {
		t.Fatalf("task 0 at task 1's forward address %s = %+v, %v; want its own task-0", peer, result, err)
	}

	// A process still in a namespace keeps it; once it is gone, the namespace
	// ends and leaves nothing.
	for index, task := range tasks {
		if err := manager.Stop(ctx, task.runtime); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := task.namespace.Stop(short)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "still holds processes") {
			t.Fatalf("task %d: Stop with a server inside = %v", index, err)
		}
		task.marker()
		holder, slirp := task.namespace.record.HolderPID, task.namespace.record.SlirpPID
		if err := task.namespace.Stop(ctx); err != nil {
			t.Fatalf("task %d: Stop: %v", index, err)
		}
		if members, err := processesInNetwork("/proc", task.namespace.record.Network); err != nil || len(members) != 0 {
			t.Fatalf("task %d: processes left in its namespace: %v, %v", index, members, err)
		}
		for _, pid := range []int{holder, slirp} {
			if _, alive, err := procStart(pid); err != nil || alive {
				t.Fatalf("task %d: process %d left: %v", index, pid, err)
			}
		}
		if _, err := os.Stat(task.namespace.directory); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("task %d: record left: %v", index, err)
		}
	}
}
