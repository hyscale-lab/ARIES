// aries-bridge serves one fixed sandbox assignment in a separately owned runtime.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	"github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: aries-bridge config.json")
	}
	info, err := os.Lstat(os.Args[1])
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return fmt.Errorf("invalid private bridge launch configuration")
	}
	data, err := os.ReadFile(os.Args[1])
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
