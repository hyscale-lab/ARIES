package apptainer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// runNamespaceEnv marks a process that runs in its run's namespace. Its
	// value is the network namespace the parent ran in, which the child's
	// must differ from, and the descriptor that releases the child once the
	// namespace has internet access.
	runNamespaceEnv = "ARIES_RUN_NAMESPACE"
	runNamespaceTap = "tap0"
	slirpReadyWait  = 30 * time.Second
	// slirpDNS is slirp4netns's forwarder to the host's name servers.
	slirpDNS = "10.0.2.3"
)

// RunNamespaceDNS returns the name server instances should use: slirp4netns's
// forwarder inside a run namespace, which reaches the host's name servers even
// when they listen on the host's loopback, and nothing elsewhere.
func RunNamespaceDNS() []string {
	if _, err := currentRunNamespace(); err != nil {
		return nil
	}
	return []string{slirpDNS}
}

// RunInNamespace runs argv in a new user and network namespace and returns its
// exit status. The namespace maps the invoking user to itself, so apptainer
// inside behaves as for a normal user, and slirp4netns gives it internet
// access but not the host's loopback. Nothing in it can be reached from
// outside: the run's control and SSH ports, which have no authentication, stay
// private on a host shared with other users. Apptainer cannot exec into an
// instance from outside the namespace that started it, so the whole run,
// Runner and bridge included, lives inside.
func RunInNamespace(ctx context.Context, argv []string, slirpPath string) (int, error) {
	if len(argv) == 0 {
		return -1, errors.New("run namespace requires a command")
	}
	if slirpPath == "" {
		slirpPath = "slirp4netns"
	}
	slirp, err := exec.LookPath(slirpPath)
	if err != nil {
		return -1, fmt.Errorf("the Apptainer run namespace needs slirp4netns for internet access: %w", err)
	}
	parentNetwork, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return -1, fmt.Errorf("read this process's network namespace: %w", err)
	}
	release, releaseWriter, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	defer release.Close()
	defer releaseWriter.Close()

	child := exec.Command(argv[0], argv[1:]...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The release pipe is the child's descriptor 3.
	child.ExtraFiles = []*os.File{release}
	child.Env = append(withoutRunNamespace(os.Environ()), runNamespaceEnv+"="+parentNetwork+";3")
	uid, gid := os.Getuid(), os.Getgid()
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := child.Start(); err != nil {
		return -1, fmt.Errorf("start the run in a new user and network namespace: %w", err)
	}
	_ = release.Close()
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()

	stopChild := func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-childDone:
		case <-time.After(30 * time.Second):
			_ = child.Process.Kill()
			<-childDone
		}
	}
	network, ready, err := startSlirp(slirp, child.Process.Pid)
	if err != nil {
		_ = releaseWriter.Close()
		stopChild()
		return -1, err
	}
	defer func() {
		_ = network.Process.Signal(syscall.SIGTERM)
		_ = network.Wait()
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = releaseWriter.Close()
			stopChild()
			return -1, err
		}
	case <-time.After(slirpReadyWait):
		_ = releaseWriter.Close()
		stopChild()
		return -1, errors.New("slirp4netns did not configure the run namespace in time")
	}
	if _, err := releaseWriter.Write([]byte{1}); err != nil {
		stopChild()
		return -1, fmt.Errorf("release the run namespace: %w", err)
	}
	_ = releaseWriter.Close()

	var waitErr error
	select {
	case waitErr = <-childDone:
	case <-ctx.Done():
		// Let the run clean up its runtimes before the namespace goes.
		_ = child.Process.Signal(syscall.SIGTERM)
		waitErr = <-childDone
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		return exit.ExitCode(), nil
	}
	if waitErr != nil {
		return -1, waitErr
	}
	return 0, nil
}

// startSlirp attaches slirp4netns to the namespace of pid. The returned channel
// reports once slirp4netns has configured the interface, or why it could not.
func startSlirp(path string, pid int) (*exec.Cmd, <-chan error, error) {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	network := exec.Command(path, "--configure", "--mtu=65520", "--disable-host-loopback", "--ready-fd=3", strconv.Itoa(pid), runNamespaceTap)
	network.ExtraFiles = []*os.File{readyWriter}
	var output strings.Builder
	network.Stdout, network.Stderr = &output, &output
	if err := network.Start(); err != nil {
		readyReader.Close()
		readyWriter.Close()
		return nil, nil, fmt.Errorf("start slirp4netns: %w", err)
	}
	readyWriter.Close()
	ready := make(chan error, 1)
	go func() {
		defer readyReader.Close()
		buffer := make([]byte, 1)
		if n, err := readyReader.Read(buffer); n == 1 && buffer[0] == '1' {
			ready <- nil
			return
		} else if err != nil {
			ready <- fmt.Errorf("slirp4netns did not configure the run namespace: %w: %s", err, strings.TrimSpace(output.String()))
			return
		}
		ready <- errors.New("slirp4netns reported an unexpected readiness value")
	}()
	return network, ready, nil
}

// WaitForRunNamespace is called first by a process that RunInNamespace
// started. It waits until the namespace has internet access and confirms the
// process is not in its parent's network namespace. A process started any
// other way returns false.
func WaitForRunNamespace() (bool, error) {
	value, ok := os.LookupEnv(runNamespaceEnv)
	if !ok {
		return false, nil
	}
	parentNetwork, descriptor, found := strings.Cut(value, ";")
	if !found || descriptor != "3" || parentNetwork == "" {
		return true, fmt.Errorf("invalid %s value", runNamespaceEnv)
	}
	release := os.NewFile(3, "aries-run-namespace")
	if release == nil {
		return true, errors.New("run namespace release descriptor is missing")
	}
	buffer := make([]byte, 1)
	n, err := release.Read(buffer)
	_ = release.Close()
	if n != 1 {
		return true, fmt.Errorf("run namespace was not released: %v", err)
	}
	// Later readers only compare namespaces; the descriptor is spent.
	if err := os.Setenv(runNamespaceEnv, parentNetwork); err != nil {
		return true, err
	}
	if _, err := currentRunNamespace(); err != nil {
		return true, err
	}
	if _, err := net.InterfaceByName(runNamespaceTap); err != nil {
		return true, fmt.Errorf("run namespace has no %s interface: %w", runNamespaceTap, err)
	}
	return true, nil
}

// currentRunNamespace returns this process's network namespace when it is a
// run namespace that RunInNamespace prepared.
func currentRunNamespace() (string, error) {
	value, ok := os.LookupEnv(runNamespaceEnv)
	if !ok {
		return "", errors.New("Apptainer runs need their own network namespace; start aries so that it enters one")
	}
	parentNetwork, _, _ := strings.Cut(value, ";")
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return "", fmt.Errorf("read this process's network namespace: %w", err)
	}
	if current == parentNetwork {
		return "", errors.New("this process shares its parent's network namespace")
	}
	return current, nil
}

func withoutRunNamespace(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, runNamespaceEnv+"=") {
			result = append(result, entry)
		}
	}
	return result
}
