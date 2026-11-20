// aries-bridge serves one fixed sandbox assignment in a separately owned runtime.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	"github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/replay/managed"
	"github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	flags := flag.NewFlagSet("aries-bridge", flag.ContinueOnError)
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := flags.Args()
	if len(args) == 2 && args[0] == "replay" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return managed.RunChild(ctx, args[1])
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: aries-bridge config.json | aries-bridge replay config.json")
	}
	info, err := os.Lstat(args[0])
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return fmt.Errorf("invalid private bridge launch configuration")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var config bridge.LaunchConfig
	if err = json.Unmarshal(data, &config); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	return bridgewiring.ServeChild(ctx, config, logrus.New())
}
