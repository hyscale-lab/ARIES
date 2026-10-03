package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// maxCollectedLog bounds one collected log. The bridges cap their own logs
// well below this.
const maxCollectedLog = 1 << 30

// Credentials is the runner's half of one grant: it holds the client private
// key (and for OpenClaw the aries-ssh helper) and turns the bridge's answer
// into the endpoint the harness is given. hermesssh.Credentials and
// openclawssh.Credentials implement it.
type Credentials interface {
	AuthorizedKey() ssh.PublicKey
	Endpoint(address string, hostKey ssh.PublicKey, network string) (core.ToolEndpoint, error)
	Revoke() error
}

// CredentialsFactory creates one grant's credentials under artifactDir.
type CredentialsFactory func(artifactDir string) (Credentials, error)

// Options configure the runner side of a remote bridge.
type Options struct {
	BridgeType     string
	Transport      Transport
	OutputDir      string
	RetainRawLog   bool
	NewCredentials CredentialsFactory
	Logger         *logrus.Logger
}

// Client is one task's grant on the remote bridge. It implements
// runner.ToolBridge, so the runner cannot tell it from an in-process bridge.
type Client struct {
	bridgeType     string
	transport      Transport
	outputDir      string
	retainRawLog   bool
	newCredentials CredentialsFactory
	logger         *logrus.Logger

	mu          sync.Mutex
	started     bool
	credentials Credentials
	artifactDir string
	grantID     string
	target      Target
	sandbox     SandboxRef
	// joined is set once Join may have changed something Leave must undo.
	joined bool
	left   bool
	// requested is set once a grant request may have reached the daemon;
	// from then on Stop must obtain positive revocation.
	requested bool
	granted   bool
	logFiles  []string
	revoked   bool
	lost      error
	keysGone  bool
	collected bool
	done      bool
}

// New constructs one task's remote bridge without contacting the bridge.
func New(options Options) (*Client, error) {
	switch options.BridgeType {
	case "hermes-ssh", "openclaw-ssh":
	default:
		return nil, fmt.Errorf("bridge type %q cannot run as a separate bridge", options.BridgeType)
	}
	if options.Transport == nil || options.OutputDir == "" || options.NewCredentials == nil {
		return nil, errors.New("remote bridge needs a transport, an output directory and a credentials factory")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, err
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Client{
		bridgeType: options.BridgeType, transport: options.Transport, outputDir: outputDir,
		retainRawLog: options.RetainRawLog, newCredentials: options.NewCredentials, logger: options.Logger,
	}, nil
}

// Preflight finds the bridge and checks its daemon answers, so a run with no
// bridge deployed fails before its first sandbox is created.
func Preflight(ctx context.Context, options Options) error {
	client, err := New(options)
	if err != nil {
		return err
	}
	target, err := client.transport.Locate(ctx)
	if err != nil {
		return err
	}
	response, err := client.call(ctx, target, Request{Op: OpStatus})
	if err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("bridge %s: %s", target.Name, response.Error)
	}
	client.logger.WithFields(logrus.Fields{"bridge": target.Name, "instance": response.Instance, "grants": response.Grants}).Info("bridge ready")
	return nil
}

// Start asks the bridge to serve this task's sandbox.
func (c *Client) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return core.ToolEndpoint{}, errors.New("remote bridge grant already started")
	}
	sandbox, err := c.transport.Describe(generic)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	c.started = true
	c.sandbox = sandbox
	c.artifactDir = filepath.Join(c.outputDir, sandbox.TaskID, "bridge")
	credentials, err := c.newCredentials(c.artifactDir)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	c.credentials = credentials
	if c.grantID, err = randomHex(16); err != nil {
		return core.ToolEndpoint{}, err
	}
	if c.target, err = c.transport.Locate(ctx); err != nil {
		return core.ToolEndpoint{}, err
	}
	c.joined = true
	listen, err := c.transport.Join(ctx, c.target, sandbox)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("make bridge %s reachable from the task: %w", c.target.Name, err)
	}
	c.requested = true
	response, err := c.call(ctx, c.target, Request{Op: OpGrant, GrantID: c.grantID, Grant: &GrantRequest{
		BridgeType: c.bridgeType, Sandbox: sandbox, ListenHost: listen,
		AuthorizedKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(credentials.AuthorizedKey()))),
		RetainRawLog:  c.retainRawLog,
	}})
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("grant on bridge %s: %w", c.target.Name, err)
	}
	if response.Error != "" || response.Grant == nil {
		return core.ToolEndpoint{}, fmt.Errorf("bridge %s refused the grant: %s", c.target.Name, response.Error)
	}
	c.granted = true
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(response.Grant.HostKey))
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("bridge returned an unusable host key: %w", err)
	}
	for _, name := range response.Grant.LogFiles {
		if err := validateLogFile(name); err != nil {
			return core.ToolEndpoint{}, err
		}
		c.logFiles = append(c.logFiles, name)
	}
	endpoint, err := credentials.Endpoint(response.Grant.Address, hostKey, response.Grant.Network)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	for _, name := range c.logFiles {
		endpoint.LogPaths = append(endpoint.LogPaths, filepath.Join(c.artifactDir, name))
	}
	c.logger.WithContext(ctx).WithFields(logrus.Fields{
		"address": endpoint.Address, "bridge": c.target.Name, "instance": response.Instance, "grant_id": c.grantID,
	}).Info("remote bridge granted")
	return endpoint, nil
}

// Stop revokes the grant, then brings its evidence back. A nil error means
// the bridge positively confirmed that no session is being served, whatever
// Join set up is undone, the runner's copy of the private key is gone, and
// the logs are on this host. Each step is remembered, so a retry resumes
// where the last attempt stopped.
func (c *Client) Stop(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.done {
		return nil
	}
	if c.requested && !c.revoked {
		if err := c.revoke(ctx); err != nil {
			return err
		}
		c.revoked = true
	}
	// Only after revocation: until then the bridge may still be serving the
	// harness over what Join set up.
	if c.joined && !c.left {
		if err := c.transport.Leave(ctx, c.target, c.sandbox); err != nil {
			return fmt.Errorf("detach bridge %s from the task: %w", c.target.Name, err)
		}
		c.left = true
	}
	if c.credentials != nil && !c.keysGone {
		if err := c.credentials.Revoke(); err != nil {
			return fmt.Errorf("remove bridge credentials: %w", err)
		}
		c.keysGone = true
	}
	if c.lost != nil {
		// Access is revoked, but the evidence cannot come back. That blocks
		// evaluation exactly as a bridge that failed to drain its logs would.
		return c.lost
	}
	if c.granted && !c.collected {
		if err := c.collect(ctx); err != nil {
			return err
		}
		c.collected = true
		// The logs are safe here, so a failed release only leaves disk used
		// in the bridge; it does not block evaluation.
		if response, err := c.call(ctx, c.target, Request{Op: OpRelease, GrantID: c.grantID}); err != nil || response.Error != "" {
			c.logger.WithFields(logrus.Fields{"grant_id": c.grantID, "error": errors.Join(err, responseError(response))}).Warn("bridge did not release a collected grant")
		}
	}
	c.done = true
	return nil
}

// revoke obtains positive confirmation that the bridge serves no session for
// this grant. Either the daemon says so, or the process that held the grant
// is provably gone: sessions live only in its memory.
func (c *Client) revoke(ctx context.Context) error {
	if c.target.Name == "" {
		// The bridge was never located, so no request reached any bridge.
		return nil
	}
	response, callErr := c.call(ctx, c.target, Request{Op: OpRevoke, GrantID: c.grantID})
	if callErr == nil {
		if response.Error != "" {
			return fmt.Errorf("bridge %s did not confirm revocation: %s", c.target.Name, response.Error)
		}
		switch response.State {
		case StateRevoked:
			return nil
		case StateAbsent:
			if c.granted {
				c.lost = fmt.Errorf("bridge %s (instance %s) no longer holds grant %s; access is revoked but its tool-call logs are lost", c.target.Name, response.Instance, c.grantID)
			}
			return nil
		}
		return fmt.Errorf("bridge %s reported grant state %q after revoke", c.target.Name, response.State)
	}
	gone, err := c.transport.Gone(ctx, c.target)
	if err != nil {
		return errors.Join(fmt.Errorf("revoke on bridge %s: %w", c.target.Name, callErr), err)
	}
	if gone {
		if c.granted {
			c.lost = fmt.Errorf("bridge %s is gone; access is revoked but grant %s's tool-call logs are lost", c.target.Name, c.grantID)
		}
		return nil
	}
	return fmt.Errorf("revocation not confirmed: bridge %s is still present but did not answer: %w", c.target.Name, callErr)
}

// collect streams the grant's logs into a private temporary file and then
// writes each expected log under its own name.
func (c *Client) collect(ctx context.Context) error {
	archive, err := os.CreateTemp(c.artifactDir, ".collect-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := c.transport.Exec(ctx, c.target, nil, archive, "collect", c.grantID); err != nil {
		return fmt.Errorf("collect logs from bridge %s: %w", c.target.Name, err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	expected := make(map[string]bool, len(c.logFiles))
	for _, name := range c.logFiles {
		expected[name] = true
	}
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read collected logs: %w", err)
		}
		if !expected[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maxCollectedLog {
			return fmt.Errorf("bridge sent an unexpected entry %q", header.Name)
		}
		if err := writePrivate(filepath.Join(c.artifactDir, header.Name), io.LimitReader(reader, header.Size), header.Size); err != nil {
			return err
		}
		delete(expected, header.Name)
	}
	for name := range expected {
		return fmt.Errorf("bridge did not send %s", name)
	}
	return nil
}

// writePrivate replaces path atomically with exactly size bytes, mode 0600.
func writePrivate(path string, content io.Reader, size int64) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	written, err := io.Copy(temporary, content)
	if err == nil && written != size {
		err = fmt.Errorf("%s: received %d of %d bytes", filepath.Base(path), written, size)
	}
	if err == nil {
		err = temporary.Chmod(0o600)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

func (c *Client) call(ctx context.Context, target Target, request Request) (Response, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	var stdout bytes.Buffer
	if err := c.transport.Exec(ctx, target, bytes.NewReader(encoded), &limitedWriter{writer: &stdout, remaining: maxMessageBytes}, "ctl"); err != nil {
		return Response{}, err
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return Response{}, fmt.Errorf("parse bridge response: %w", err)
	}
	if response.Instance == "" {
		return Response{}, errors.New("bridge response names no instance")
	}
	return response, nil
}

func responseError(response Response) error {
	if response.Error == "" {
		return nil
	}
	return errors.New(response.Error)
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(buffer []byte) (int, error) {
	if len(buffer) > w.remaining {
		return 0, errors.New("bridge response too large")
	}
	w.remaining -= len(buffer)
	return w.writer.Write(buffer)
}

// truncatingWriter keeps the first bytes and drops the rest without failing,
// so a noisy stderr cannot turn into a command failure.
type truncatingWriter struct {
	writer    io.Writer
	remaining int
}

func (w *truncatingWriter) Write(buffer []byte) (int, error) {
	if kept := min(len(buffer), w.remaining); kept > 0 {
		_, _ = w.writer.Write(buffer[:kept])
		w.remaining -= kept
	}
	return len(buffer), nil
}
