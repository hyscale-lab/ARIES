// Dispatch reviewed script files into one already-owned pilot Docker sandbox.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bridge "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"golang.org/x/crypto/ssh"
)

func run() (err error) {
	if len(os.Args) < 10 || (len(os.Args)-7)%3 != 0 {
		return errors.New("usage: aries-bridge-pilot RUN TASK NAME IMMUTABLE_ID WORKDIR OUTPUT START_OFFSET EXPECTED_EXIT SCRIPT [START_OFFSET EXPECTED_EXIT SCRIPT ...]")
	}
	runID, task, name, id, dir, out := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	ctx := context.Background()
	backend, err := docker.New(docker.Options{Socket: os.Getenv("DOCKER_HOST")})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, backend.Close()) }()
	borrowed, err := target.New(ctx, core.BridgeTarget{RunID: runID, TaskID: task, SandboxID: name, RuntimeID: id, Backend: "docker", Workdir: dir}, backend)
	if err != nil {
		return err
	}
	defer borrowed.Revoke()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	server, err := openclaw.New(bridge.Options{HostSigner: signer, SandboxID: name, OutputDir: out, ResolveListen: func(context.Context) (core.BridgeListen, error) { return core.BridgeListen{BindHost: "127.0.0.1"}, nil }})
	if err != nil {
		return err
	}
	ep, err := server.StartTarget(ctx, borrowed)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, server.Stop(ctx)) }()
	client, err := ssh.Dial("tcp", ep.Address, &ssh.ClientConfig{User: ep.Username, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer client.Close()
	start := time.Now()
	for i := 7; i < len(os.Args); i += 3 {
		offset, e := time.ParseDuration(os.Args[i])
		if e != nil || offset < 0 {
			return errors.New("start offset must be a nonnegative duration")
		}
		want, e := strconv.Atoi(os.Args[i+1])
		if e != nil || want < 0 || want > 255 {
			return errors.New("expected exit must be 0..255")
		}
		path := os.Args[i+2]
		time.Sleep(time.Until(start.Add(offset)))
		if err := dispatch(client, path, filepath.Join(out, fmt.Sprintf("%03d", (i-7)/3+1)), want); err != nil {
			return err
		}
	}
	return nil
}

func dispatch(client *ssh.Client, path, prefix string, want int) error {
	script, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	stdout, err := os.OpenFile(prefix+".stdout", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(prefix+".stderr", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	session.Stdout, session.Stderr = stdout, stderr
	quoted := strings.ReplaceAll(string(script), "'", "'\"'\"'")
	err = session.Run("'/bin/sh' '-c' '" + quoted + "'")
	got := 0
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		got = exit.ExitStatus()
	} else if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s: exit %d, expected %d", path, got, want)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
