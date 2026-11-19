package apptainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A task namespace holds one task's runtimes: a user and network namespace of
// their own, nested in the run's, so that tasks running at once can use the
// same ports. Its first process, the holder, keeps it in existence. The holder
// is cat reading a pipe that only Runner writes to, so it ends when Runner
// closes the pipe or dies. slirp4netns runs in the run namespace and gives the
// task internet access, forwards ports from the run namespace into it, and
// connects what the task sends to its gateway to the run namespace's loopback,
// where the bridge listens. Apptainer cannot execute in an instance from
// outside the network namespace that started it, so every apptainer command
// that starts an instance or executes in one enters the task's namespace with
// nsenter. A record in the state directory lets the bridge, a process of its
// own, enter it too.
const (
	// taskNamespacePrefix starts the placement of a runtime in a task
	// namespace; the rest names the namespace's directory in the state
	// directory.
	taskNamespacePrefix = "task-namespace:"
	// taskNetwork is the subnet slirp4netns gives each task. It differs from
	// the run namespace's 10.0.2.0/24, so 10.0.2.3, the run's name server
	// forwarder, is an address the task routes to: a task's own forwarder runs
	// in the run namespace, where the host's resolver on 127.0.0.53 cannot be
	// reached.
	taskNetwork = "10.0.3.0/24"
	// taskGateway is the task's gateway. slirp4netns connects what a task sends
	// to it to the run namespace's loopback.
	taskGateway = "10.0.3.2"
	// taskGuest is the task namespace's own address, where forwarded ports
	// arrive.
	taskGuest     = "10.0.3.100"
	namespaceFile = "namespace.json"
	slirpAPIFile  = "slirp.sock"
	// maxSocketPath is the longest path a Unix socket's address holds.
	maxSocketPath = 107
	// namespaceWait bounds each step of ending a task namespace.
	namespaceWait = 5 * time.Second
	slirpAPIWait  = 10 * time.Second
	// slirpOutputWait bounds how long Wait waits for slirp4netns's output after
	// slirp4netns exits, should another process hold its output pipe.
	slirpOutputWait = time.Second
)

// taskNamespace is a task namespace's record, which every process of the run
// reads from the state directory.
type taskNamespace struct {
	Name    string            `json:"name"`
	Runtime string            `json:"runtime"`
	Labels  map[string]string `json:"labels,omitempty"`
	// HolderStart distinguishes the holder from a later process that reuses its
	// PID; Network is the namespace's identity in /proc.
	HolderPID   int    `json:"holder_pid"`
	HolderStart uint64 `json:"holder_start"`
	Network     string `json:"network"`
	SlirpPID    int    `json:"slirp_pid"`
	SlirpStart  uint64 `json:"slirp_start"`
	// Loopback is the run namespace's address at which the task's forwarded
	// ports are reached, unique to the task.
	Loopback string `json:"loopback"`
}

// taskNamespaceName returns the namespace a placement names, if any.
func taskNamespaceName(attachment string) (string, bool) {
	return strings.CutPrefix(attachment, taskNamespacePrefix)
}

func loadTaskNamespace(stateDir, name string) (taskNamespace, error) {
	if err := validName(name); err != nil {
		return taskNamespace{}, err
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, name, namespaceFile))
	if err != nil {
		return taskNamespace{}, fmt.Errorf("read task namespace %s: %w", name, err)
	}
	var current taskNamespace
	if err := json.Unmarshal(raw, &current); err != nil {
		return taskNamespace{}, fmt.Errorf("decode task namespace %s: %w", name, err)
	}
	if current.Name != name {
		return taskNamespace{}, errors.New("task namespace record names another namespace")
	}
	return current, nil
}

// taskLoopback returns the loopback address of the run's nth task namespace,
// counting from 1: 127.1.0.1, 127.1.0.2 and so on. Every address in 127.0.0.0/8
// reaches the run namespace's loopback without being configured.
func taskLoopback(n uint32) (string, error) {
	if n == 0 || n >= 254<<16 {
		return "", errors.New("the run has used every task loopback address")
	}
	return fmt.Sprintf("127.%d.%d.%d", 1+(n>>16), (n>>8)&0xff, n&0xff), nil
}

// taskNamespaceRequest describes the namespace one task handle needs.
type taskNamespaceRequest struct {
	stateDir string
	runtime  string
	labels   map[string]string
	loopback string
}

// taskNamespaceHandle is a task namespace that this process started.
type taskNamespaceHandle interface {
	Name() string
	Validate() error
	Stop(context.Context) error
}

// runningTaskNamespace is a task namespace this process started and owns.
type runningTaskNamespace struct {
	mu        sync.Mutex
	record    taskNamespace
	directory string
	procRoot  string
	// release is the holder's input; closing it ends the holder. slirpExit is
	// slirp4netns's exit pipe; closing it ends slirp4netns.
	release, slirpExit    *os.File
	holder, slirp         *os.Process
	holderDone, slirpDone chan error
	stopped               bool
}

// CheckStateDir refuses a state directory too long for the path of a task's
// slirp4netns socket, <state_dir>/ns-<16 hex digits>/slirp.sock, which must fit
// a Unix socket address.
func CheckStateDir(stateDir string) error {
	api := filepath.Join(stateDir, "ns-"+strings.Repeat("0", 16), slirpAPIFile)
	if len(api) > maxSocketPath {
		return fmt.Errorf("Apptainer state directory %s is too long: the path of a task's slirp4netns socket, such as %s, would exceed %d bytes", stateDir, api, maxSocketPath)
	}
	return nil
}

// startTaskNamespace starts a task namespace and its slirp4netns, and records
// both in the state directory. When it fails after starting something and
// cannot end it, it returns the namespace with the error, for Stop to end.
func startTaskNamespace(ctx context.Context, request taskNamespaceRequest) (taskNamespaceHandle, error) {
	if err := CheckStateDir(request.stateDir); err != nil {
		return nil, err
	}
	nonce, err := randomID()
	if err != nil {
		return nil, err
	}
	name := "ns-" + nonce
	directory := filepath.Join(request.stateDir, name)
	api := filepath.Join(directory, slirpAPIFile)
	holderPath, err := exec.LookPath("cat")
	if err != nil {
		return nil, fmt.Errorf("a task namespace's holder runs cat: %w", err)
	}
	slirpPath, err := exec.LookPath("slirp4netns")
	if err != nil {
		return nil, fmt.Errorf("a task namespace needs slirp4netns for internet access: %w", err)
	}
	if err := os.MkdirAll(request.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Apptainer state directory: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("reserve task namespace %s: %w", name, err)
	}
	namespace := &runningTaskNamespace{
		record:    taskNamespace{Name: name, Runtime: request.runtime, Labels: request.labels, Loopback: request.loopback},
		directory: directory, procRoot: "/proc",
	}
	if err := namespace.start(ctx, holderPath, slirpPath, api); err != nil {
		// Ending what started is bounded on its own, whatever is left of the
		// caller's context.
		cleanup, cancel := context.WithTimeout(context.Background(), 4*namespaceWait)
		defer cancel()
		if endErr := namespace.end(cleanup); endErr != nil {
			return namespace, errors.Join(err, endErr)
		}
		return nil, err
	}
	return namespace, nil
}

func (n *runningTaskNamespace) start(ctx context.Context, holderPath, slirpPath, api string) error {
	input, release, err := os.Pipe()
	if err != nil {
		return err
	}
	holder := exec.Command(holderPath)
	holder.Stdin = input
	uid, gid := os.Getuid(), os.Getgid()
	holder.SysProcAttr = &syscall.SysProcAttr{
		// A process group of its own keeps a terminal's interrupt, meant for
		// aries, from ending the namespace while aries cleans up in it.
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	err = holder.Start()
	_ = input.Close()
	if err != nil {
		_ = release.Close()
		return fmt.Errorf("start a task namespace: %w", err)
	}
	n.release, n.holder, n.holderDone = release, holder.Process, make(chan error, 1)
	go func() { n.holderDone <- holder.Wait() }()
	pid := holder.Process.Pid
	started, alive, err := procStart(pid)
	if err != nil {
		return fmt.Errorf("inspect the task namespace's holder: %w", err)
	}
	if !alive {
		return errors.New("the task namespace's holder exited as it started")
	}
	network, err := os.Readlink(filepath.Join(n.procRoot, strconv.Itoa(pid), "ns", "net"))
	if err != nil {
		return fmt.Errorf("read the task's network namespace: %w", err)
	}
	n.record.HolderPID, n.record.HolderStart, n.record.Network = pid, started, network

	ready, readyWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer ready.Close()
	exitReader, slirpExit, err := os.Pipe()
	if err != nil {
		_ = readyWriter.Close()
		return err
	}
	output := &boundedBuffer{limit: 4096}
	slirp := exec.Command(slirpPath, "--configure", "--mtu=65520", "--cidr="+taskNetwork, "--api-socket="+api,
		"--ready-fd=3", "--exit-fd=4", strconv.Itoa(pid), runNamespaceTap)
	slirp.ExtraFiles = []*os.File{readyWriter, exitReader}
	slirp.Stdout, slirp.Stderr = output, output
	slirp.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	slirp.WaitDelay = slirpOutputWait
	err = slirp.Start()
	_ = readyWriter.Close()
	_ = exitReader.Close()
	if err != nil {
		_ = slirpExit.Close()
		return fmt.Errorf("start slirp4netns: %w", err)
	}
	n.slirpExit, n.slirp, n.slirpDone = slirpExit, slirp.Process, make(chan error, 1)
	// Wait also waits for the output to be copied, so output is read only after
	// slirp4netns has been reaped.
	exited := make(chan error, 1)
	go func() {
		err := slirp.Wait()
		exited <- err
		n.slirpDone <- err
	}()
	configured := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		if count, err := ready.Read(buffer); count == 1 && buffer[0] == '1' {
			configured <- nil
		} else {
			configured <- fmt.Errorf("slirp4netns did not configure the task namespace: %v", err)
		}
	}()
	select {
	case err := <-configured:
		if err != nil {
			// end, which follows, still finds slirpDone filled. The output is
			// whole only once Wait has returned.
			_ = slirp.Process.Kill()
			select {
			case <-exited:
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
			case <-time.After(namespaceWait):
				return err
			}
		}
	case <-time.After(slirpReadyWait):
		return errors.New("slirp4netns did not configure the task namespace in time")
	case <-ctx.Done():
		return ctx.Err()
	}
	slirpStarted, alive, err := procStart(slirp.Process.Pid)
	if err != nil || !alive {
		return errors.Join(err, errors.New("slirp4netns exited after configuring the task namespace"))
	}
	n.record.SlirpPID, n.record.SlirpStart = slirp.Process.Pid, slirpStarted
	raw, err := json.MarshalIndent(n.record, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(n.directory, namespaceFile), raw); err != nil {
		return fmt.Errorf("write task namespace record: %w", err)
	}
	return nil
}

func (n *runningTaskNamespace) Name() string { return n.record.Name }

// Validate confirms the holder and slirp4netns are still the processes that
// were started.
func (n *runningTaskNamespace) Validate() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return errors.New("task namespace is stopped")
	}
	for _, process := range []struct {
		role  string
		pid   int
		start uint64
	}{{"holder", n.record.HolderPID, n.record.HolderStart}, {"slirp4netns", n.record.SlirpPID, n.record.SlirpStart}} {
		started, alive, err := procStart(process.pid)
		if err != nil {
			return fmt.Errorf("inspect task namespace %s: %w", n.record.Name, err)
		}
		if !alive || started != process.start {
			return fmt.Errorf("task namespace %s lost its %s", n.record.Name, process.role)
		}
	}
	return nil
}

// Stop ends the namespace once its runtimes are gone. A process still in it
// belongs to a runtime whose removal failed; it is reported, not killed, and
// Stop can be repeated after that runtime's removal.
func (n *runningTaskNamespace) Stop(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped {
		return nil
	}
	others, err := n.waitForOthers(ctx)
	if err != nil {
		return err
	}
	if len(others) != 0 {
		return fmt.Errorf("task namespace %s still holds processes %v", n.record.Name, others)
	}
	if err := n.end(ctx); err != nil {
		return err
	}
	n.stopped = true
	return nil
}

// waitForOthers waits briefly for processes other than the holder to leave
// the namespace: the processes of an instance that was just stopped end as
// the kernel tears down its PID namespace. It returns those that remain.
func (n *runningTaskNamespace) waitForOthers(ctx context.Context) ([]int, error) {
	deadline := time.Now().Add(namespaceWait)
	for {
		members, err := processesInNetwork(n.procRoot, n.record.Network)
		if err != nil {
			return nil, fmt.Errorf("list task namespace %s: %w", n.record.Name, err)
		}
		others := slices.DeleteFunc(members, func(pid int) bool { return pid == n.record.HolderPID })
		if len(others) == 0 || time.Now().After(deadline) {
			return others, nil
		}
		select {
		case <-ctx.Done():
			return others, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// end stops the holder, confirms the namespace is empty, stops slirp4netns and
// removes the record. It also undoes a start that failed part way. It returns
// when ctx ends, leaving what it could not confirm for a later end.
func (n *runningTaskNamespace) end(ctx context.Context) error {
	var errs []error
	// The holder and slirp4netns stay pending until their exits are confirmed,
	// so a later end waits for them again rather than removing the record of
	// a namespace one of them still keeps.
	if n.release != nil {
		_ = n.release.Close()
		if err := finish(ctx, n.holder, n.holderDone, "the task namespace's holder"); err != nil {
			errs = append(errs, err)
		} else {
			n.release = nil
		}
	}
	if n.record.Network != "" {
		if members, err := processesInNetwork(n.procRoot, n.record.Network); err != nil {
			errs = append(errs, err)
		} else if len(members) != 0 {
			errs = append(errs, fmt.Errorf("task namespace %s still holds processes %v after its holder ended", n.record.Name, members))
		}
	}
	if n.slirpExit != nil {
		_ = n.slirpExit.Close()
		if err := finish(ctx, n.slirp, n.slirpDone, "slirp4netns"); err != nil {
			errs = append(errs, err)
		} else {
			n.slirpExit = nil
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := os.RemoveAll(n.directory); err != nil {
		return fmt.Errorf("remove task namespace %s: %w", n.record.Name, err)
	}
	if _, err := os.Lstat(n.directory); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("task namespace %s remains after removal", n.record.Name)
	}
	return nil
}

// finish waits for a process that was asked to exit, killing it if it does
// not, and gives up when ctx ends.
func finish(ctx context.Context, process *os.Process, done <-chan error, role string) error {
	if process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(namespaceWait):
	case <-ctx.Done():
		return fmt.Errorf("%s has not exited: %w", role, ctx.Err())
	}
	_ = process.Kill()
	select {
	case <-done:
		return nil
	case <-time.After(namespaceWait):
		return fmt.Errorf("%s remains after kill", role)
	case <-ctx.Done():
		return fmt.Errorf("%s has not exited after kill: %w", role, ctx.Err())
	}
}

// processesInNetwork returns this user's processes in a network namespace.
// Another user's processes cannot be inspected and are skipped, as are those
// that exit meanwhile.
func processesInNetwork(procRoot, network string) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var members []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if link, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "ns", "net")); err == nil && link == network {
			members = append(members, pid)
		}
	}
	return members, nil
}

// slirpForward is one port forward that slirp4netns lists.
type slirpForward struct {
	Proto     string `json:"proto"`
	HostAddr  string `json:"host_addr"`
	HostPort  int    `json:"host_port"`
	GuestAddr string `json:"guest_addr"`
	GuestPort int    `json:"guest_port"`
}

// forwardPort makes a port of a task namespace reachable from the run
// namespace, at the task's own loopback address, through slirp4netns, and
// returns that address. A forward that exists already, as when another process
// made it, is reused.
func forwardPort(ctx context.Context, stateDir string, namespace taskNamespace, port int) (string, error) {
	api := filepath.Join(stateDir, namespace.Name, slirpAPIFile)
	address := net.JoinHostPort(namespace.Loopback, strconv.Itoa(port))
	exists := func() (bool, error) {
		// slirp4netns 1.2 lists its forwards without the "return" that wraps its
		// other replies; both forms are read.
		var listing struct {
			Entries []slirpForward `json:"entries"`
			Return  struct {
				Entries []slirpForward `json:"entries"`
			} `json:"return"`
		}
		if err := callSlirp(ctx, api, map[string]any{"execute": "list_hostfwd"}, &listing); err != nil {
			return false, err
		}
		return slices.ContainsFunc(append(listing.Entries, listing.Return.Entries...), func(entry slirpForward) bool {
			return entry.Proto == "tcp" && entry.HostAddr == namespace.Loopback && entry.HostPort == port && entry.GuestAddr == taskGuest && entry.GuestPort == port
		}), nil
	}
	if found, err := exists(); err != nil {
		return "", fmt.Errorf("forward port %d of task namespace %s: %w", port, namespace.Name, err)
	} else if found {
		return address, nil
	}
	added := callSlirp(ctx, api, map[string]any{"execute": "add_hostfwd", "arguments": map[string]any{
		"proto": "tcp", "host_addr": namespace.Loopback, "host_port": port, "guest_addr": taskGuest, "guest_port": port,
	}}, nil)
	if added == nil {
		return address, nil
	}
	if found, err := exists(); err == nil && found {
		return address, nil
	}
	return "", fmt.Errorf("forward port %d of task namespace %s: %w", port, namespace.Name, added)
}

// callSlirp sends one request to slirp4netns's API socket and decodes the
// reply. slirp4netns reads one request, answers it and closes the connection.
func callSlirp(ctx context.Context, path string, request, reply any) error {
	ctx, cancel := context.WithTimeout(ctx, slirpAPIWait)
	defer cancel()
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("reach slirp4netns: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	// The deadline does not see the caller cancel: closing the connection
	// interrupts a read or write in progress.
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := connection.Write(raw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send slirp4netns a request: %w", err)
	}
	if unix, ok := connection.(*net.UnixConn); ok {
		_ = unix.CloseWrite()
	}
	answer, err := io.ReadAll(io.LimitReader(connection, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read slirp4netns's reply: %w", err)
	}
	var envelope struct {
		Error *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return fmt.Errorf("decode slirp4netns's reply %q: %w", answer, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("slirp4netns: %s", envelope.Error.Desc)
	}
	if reply != nil {
		return json.Unmarshal(answer, reply)
	}
	return nil
}

// enter returns the nsenter command that runs apptainer in a runtime's task
// namespace, and nothing for a runtime outside one. The holder's start time
// stops a PID that the kernel has given to another process from being entered.
func (manager *Manager) enter(attachment string) ([]string, error) {
	name, inTask := taskNamespaceName(attachment)
	if !inTask {
		return nil, nil
	}
	namespace, err := loadTaskNamespace(manager.options.StateDir, name)
	if err != nil {
		return nil, err
	}
	started, alive, err := manager.processStart(namespace.HolderPID)
	if err != nil {
		return nil, fmt.Errorf("inspect task namespace %s: %w", name, err)
	}
	if !alive || started != namespace.HolderStart {
		return nil, fmt.Errorf("task namespace %s has ended", name)
	}
	return []string{"nsenter", "--user", "--net", "--preserve-credentials", "--target", strconv.Itoa(namespace.HolderPID)}, nil
}

// entered returns the program and arguments that run path with args, through
// enter when it is given.
func entered(enter []string, path string, args []string) (string, []string) {
	if len(enter) == 0 {
		return path, args
	}
	argv := append(slices.Clone(enter[1:]), path)
	return enter[0], append(argv, args...)
}
