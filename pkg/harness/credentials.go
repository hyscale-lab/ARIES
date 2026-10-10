package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
)

// Credentials owns private bytes for one occurrence. Its owner serializes access;
// a running request uses Snapshot so confirmed cleanup can clear the owner safely.
type Credentials struct {
	label  string
	values map[string][]byte
	order  []string
	mcp    map[string][]byte
}

func NewCredentials(label string) *Credentials {
	return &Credentials{label: label, values: make(map[string][]byte)}
}

// EnvironmentAPIKeyLookup returns an owned buffer, which Load always clears.
func EnvironmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	return []byte(value), ok
}

// Load preserves required/optional policy at the caller. A missing source is not
// an error here. Invalid sources are cleared and never retained.
func (c *Credentials) Load(name, sourceEnv string, lookup func(string) ([]byte, bool)) (bool, error) {
	source, ok := lookup(sourceEnv)
	defer clear(source)
	if !ok {
		return false, nil
	}
	if len(source) == 0 || len(source) > 16<<10 {
		return true, fmt.Errorf("%s API key is empty or exceeds its bound", c.label)
	}
	if bytes.ContainsAny(source, "\x00\r\n") {
		return true, fmt.Errorf("%s API key contains NUL or a line break", c.label)
	}
	c.Set(name, source)
	return true, nil
}

// Set copies generated or otherwise validated bytes into this owner.
func (c *Credentials) Set(name string, value []byte) {
	copied := bytes.Clone(value)
	if c.values == nil {
		c.values = make(map[string][]byte)
	}
	old, exists := c.values[name]
	clear(old)
	c.values[name] = copied
	if !exists {
		c.order = append(c.order, name)
	}
}

// Get borrows bytes for immediate use. Retain a Snapshot across concurrent Stop.
func (c *Credentials) Get(name string) []byte { return c.values[name] }

// LoadMCP retains one copy per host variable, in deterministic server/key order.
func (c *Credentials) LoadMCP(servers []core.MCPServerConfig, configuration []byte, lookup func(string) ([]byte, bool)) error {
	if c.mcp == nil {
		c.mcp = make(map[string][]byte)
	}
	for _, srv := range servers {
		keys := make([]string, 0, len(srv.SecretEnv))
		for key := range srv.SecretEnv {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			hostVar := srv.SecretEnv[key]
			if _, exists := c.mcp[hostVar]; exists {
				continue
			}
			name := "mcp:" + hostVar
			ok, err := c.Load(name, hostVar, lookup)
			if err != nil {
				return fmt.Errorf("%s MCP server %q secret environment variable %q (%q): %w", c.label, srv.Name, hostVar, key, err)
			}
			if !ok {
				return fmt.Errorf("%s MCP server %q secret environment variable %q (%q) is not set", c.label, srv.Name, hostVar, key)
			}
			value := c.Get(name)
			if bytes.Contains(configuration, value) {
				return fmt.Errorf("rendered %s config contains secret for MCP server %q (%q)", c.label, srv.Name, key)
			}
			c.mcp[hostVar] = value
		}
	}
	return nil
}

// AddRedactions adds values the harness is never given but scrubs, with its
// own credentials, from everything it saves: a benchmark's credentials that
// reach the sandbox, where the agent can read and repeat them. names are host
// environment variables read through lookup; an unset one is skipped, and a
// multi-line or JSON value is scrubbed by its lines and string fields as well
// (core.SecretParts). Nothing added here is staged into the runtime.
func (c *Credentials) AddRedactions(names []string, lookup func(string) ([]byte, bool)) {
	for _, part := range core.LookupSecretParts(lookup, names) {
		c.Set(fmt.Sprintf("redact:%d", len(c.order)), part)
		clear(part)
	}
}

// MCPFiles borrows the credential buffers to stage their native filenames.
func (c *Credentials) MCPFiles() map[string][]byte { return c.mcp }

func (c *Credentials) Snapshot() *Credentials {
	copy := NewCredentials(c.label)
	for _, name := range c.order {
		copy.Set(name, c.values[name])
	}
	if c.mcp != nil {
		copy.mcp = make(map[string][]byte, len(c.mcp))
		for name := range c.mcp {
			copy.mcp[name] = copy.Get("mcp:" + name)
		}
	}
	return copy
}

func (c *Credentials) Clear() {
	if c == nil {
		return
	}
	for _, value := range c.values {
		clear(value)
	}
	c.values = nil
	c.order = nil
	c.mcp = nil
}

// Secrets borrows the values in their stable load order for deployment validation.
func (c *Credentials) Secrets() [][]byte {
	secrets := make([][]byte, 0, len(c.order))
	for _, name := range c.order {
		secrets = append(secrets, c.values[name])
	}
	return secrets
}

func (c *Credentials) Redact(content []byte) []byte {
	return privatefiles.RedactSecrets(content, c.Secrets()...)
}

type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// RedactErr retains cancellation classification without exposing the original error.
func (c *Credentials) RedactErr(err error) error {
	if err == nil {
		return nil
	}
	var classification error
	if errors.Is(err, context.Canceled) {
		classification = context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		classification = context.DeadlineExceeded
	}
	return &redactedError{message: string(c.Redact([]byte(err.Error()))), cause: classification}
}
