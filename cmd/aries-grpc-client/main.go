// Command aries-grpc-client is the client the tool bridge stages into the
// harness container for a hermes-grpc run, where the ARIES Hermes plugin runs
// it once per command; see pkg/bridge/grpc.
package main

import (
	"os"

	grpcbridge "github.com/hyscale-lab/aries/pkg/bridge/grpc"
)

func main() {
	os.Exit(grpcbridge.ClientMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
