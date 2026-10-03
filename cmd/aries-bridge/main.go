// Command aries-bridge is the tool bridge running in its own pod.
//
//	aries-bridge serve              the long-running daemon (the pod's entrypoint)
//	aries-bridge ctl                relay one JSON control request from stdin
//	aries-bridge collect GRANT_ID   write a revoked grant's logs to stdout as tar
//	aries-bridge status             exit 0 if the daemon answers (readiness probe)
//
// The runner drives ctl and collect with `kubectl exec`; see pkg/bridge/remote.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/bridge/remote"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	k8ssandbox "github.com/hyscale-lab/aries/pkg/sandbox/kubernetes"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// ctlTimeout bounds one control request. A grant attaches to the sandbox pod
// and starts an SSH listener; a revoke drains in-flight tool calls.
const ctlTimeout = 3 * time.Minute

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "aries-bridge: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: aries-bridge serve | ctl | collect GRANT_ID | status")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	socket := flags.String("socket", remote.DefaultSocket, "control socket path")
	switch args[0] {
	case "serve":
		backend := flags.String("backend", remote.BackendKubernetes, `sandboxes this bridge serves: "kubernetes" or "docker"`)
		outputDir := flags.String("output-dir", "/var/lib/aries-bridge", "container-local directory for live grants")
		namespace := flags.String("namespace", os.Getenv("POD_NAMESPACE"), "kubernetes: namespace whose sandboxes this bridge serves")
		advertise := flags.String("advertise-host", os.Getenv("POD_IP"), "kubernetes: address harness pods dial, normally this pod's IP")
		dockerSocket := flags.String("docker-socket", "/var/run/docker.sock", "docker: Engine socket used to exec into sandboxes")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return serve(*backend, *socket, *outputDir, *namespace, *advertise, *dockerSocket)
	case "ctl":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return remote.RunCtl(*socket, stdin, stdout, ctlTimeout)
	case "collect":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: aries-bridge collect GRANT_ID")
		}
		return remote.RunCollect(*socket, flags.Arg(0), stdout, ctlTimeout)
	case "status":
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		response, err := remote.Call(*socket, remote.Request{Op: remote.OpStatus}, 5*time.Second)
		if err != nil {
			return err
		}
		if response.Error != "" {
			return errors.New(response.Error)
		}
		_, err = fmt.Fprintf(stdout, "instance %s, %d grant(s)\n", response.Instance, response.Grants)
		return err
	}
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func serve(backend, socket, outputDir, namespace, advertise, dockerSocket string) error {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	// The explicit switch from deployment to sandbox attach. On Kubernetes
	// every grant advertises this pod's IP; on Docker the runner attaches the
	// container to each task's network and names the address in the grant.
	var attach remote.AttachFunc
	switch backend {
	case remote.BackendKubernetes:
		if advertise == "" {
			return errors.New("--advertise-host (or $POD_IP) is required: harness pods must be told where to dial")
		}
		attach = func(ctx context.Context, request remote.GrantRequest) (runner.Sandbox, error) {
			sandbox := request.Sandbox
			return k8ssandbox.Attach(ctx, k8ssandbox.AttachOptions{
				Namespace: sandbox.Namespace, PodName: sandbox.PodName, SandboxID: sandbox.SandboxID,
				Workdir: sandbox.Workdir, RunID: sandbox.RunID, TaskID: sandbox.TaskID, Logger: logger,
			})
		}
	case remote.BackendDocker:
		attacher, err := dockersandbox.NewAttacher(dockerSocket, logger)
		if err != nil {
			return err
		}
		defer attacher.Close()
		advertise = ""
		attach = func(ctx context.Context, request remote.GrantRequest) (runner.Sandbox, error) {
			sandbox := request.Sandbox
			return attacher.Attach(ctx, dockersandbox.AttachOptions{
				ContainerID: sandbox.ContainerID, Network: sandbox.Network,
				Workdir: sandbox.Workdir, RunID: sandbox.RunID, TaskID: sandbox.TaskID,
			})
		}
	default:
		return fmt.Errorf("unknown --backend %q", backend)
	}
	daemon, err := remote.NewDaemon(remote.DaemonOptions{
		OutputDir: outputDir, Backend: backend, Namespace: namespace, Logger: logger,
		NewBridge: newBridgeFactory(advertise, logger), Attach: attach,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.WithFields(logrus.Fields{"instance": daemon.Instance(), "backend": backend, "namespace": namespace, "advertise_host": advertise}).Info("aries-bridge serving")
	serveErr := daemon.Serve(ctx, socket)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(serveErr, daemon.Close(cleanupCtx))
}

// newBridgeFactory is the explicit switch from bridge type to concrete bridge.
// Every grant gets its own manager serving the runner-supplied key. A grant
// that names a listen host (Docker: the bridge's address on the task network)
// binds and advertises exactly that; otherwise it advertises this bridge's
// default address (Kubernetes: the pod IP) and binds every interface.
func newBridgeFactory(advertise string, logger *logrus.Logger) remote.BridgeFactory {
	return func(request remote.GrantRequest, outputDir string, host ssh.Signer, authorized ssh.PublicKey) (runner.ToolBridge, error) {
		listen, address := request.ListenHost, advertise
		if listen != "" {
			address = listen
		}
		if address == "" {
			return nil, errors.New("grant names no listen host and this bridge has no default address")
		}
		switch request.BridgeType {
		case "hermes-ssh":
			return hermesssh.New(hermesssh.Options{
				OutputDir: outputDir, Logger: logger, AdvertiseHost: address, ListenHost: listen,
				OmitRawLog: !request.RetainRawLog, Keys: &hermesssh.SessionKeys{Host: host, Authorized: authorized},
			})
		case "openclaw-ssh":
			return openclawssh.New(openclawssh.Options{
				OutputDir: outputDir, Logger: logger, AdvertiseHost: address, ListenHost: listen,
				OmitRawLog: !request.RetainRawLog, Keys: &openclawssh.SessionKeys{Host: host, Authorized: authorized},
			})
		}
		return nil, fmt.Errorf("bridge type %q cannot run as a separate bridge", request.BridgeType)
	}
}
