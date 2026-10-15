// aries-codex-ssh is the program Codex's native remote environment spawns in
// the harness container. It relays the executor's stdio to the bridge over SSH.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/client"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/codex/client"
)

func main() {
	stdin, err := sshclient.OpenStdin()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "aries-codex-ssh: open owned stdin: %v\n", err)
		os.Exit(255)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := client.Main(ctx, os.Args[1:], stdin, os.Stdout, os.Stderr)
	stop()
	_ = stdin.Close()
	os.Exit(code)
}
