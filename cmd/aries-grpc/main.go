// Command aries-grpc is the client the Hermes gRPC bridge stages into the
// harness container, where the ARIES Hermes plugin runs it once per command;
// see pkg/bridge/hermesgrpc.
package main

import (
	"os"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc"
)

func main() {
	os.Exit(hermesgrpc.ClientMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
