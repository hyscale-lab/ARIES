package ssh

import "github.com/hyscale-lab/aries/pkg/core"

// Dialect adapts harness command semantics without owning transport or execution.
type Dialect interface {
	Prepare(encoded, workdir string) (Prepared, *Refusal)
	Policy() Policy
}

type Action uint8

const (
	Execute Action = iota
	DrainOnly
)

// Prepared separates exact execution from the native evidence representation.
type Prepared struct {
	Command                                                         core.Command
	Action                                                          Action
	HashInput, Display, OperationClass, RefusalClass, WorkspaceHome string
	Environment                                                     []string
}

// Refusal distinguishes malformed input from native policy denial.
type Refusal struct {
	OperationClass, Status, Message string
}

type UnsupportedRequests uint8

const (
	RejectAndClose UnsupportedRequests = iota
	RejectAndContinue
)

type Policy struct {
	UnsupportedRequests   UnsupportedRequests
	InvalidOperationClass string
	RefusedExitCode       int
	Endpoint              EndpointPolicy
}

type EndpointPolicy struct {
	ClientCommand, KnownHostsFile string
	UseSandboxWorkdir             bool
}
