package main

import (
	"os"

	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
)

func main() {
	os.Exit(openclawssh.ClientMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
