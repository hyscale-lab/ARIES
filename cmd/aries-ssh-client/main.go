package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/client"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client"
)

func main() {
	stdin, err := sshclient.OpenStdin()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "aries-ssh-client: open owned stdin: %v\n", err)
		os.Exit(255)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := client.Main(ctx, os.Args[1:], stdin, os.Stdout, os.Stderr)
	stop()
	_ = stdin.Close()
	os.Exit(code)
}
