package toolathlon

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

// The values the adapter takes from the host environment (credentials.go)
// are replaced with a placeholder in what it saves: the preprocessing and
// grading logs, the gateway's log, the grader's result, the task bundle, and
// the errors and evaluation details it returns. Toolathlon's servers and
// scripts log request headers and print what they read, so a token given to
// them can reach any of these, and run directories may be shared. The
// harness scrubs the same values from what it saves (wiring passes it their
// names), since the agent can read them in the sandbox.

// redactedValue replaces every scrubbed occurrence.
const redactedValue = "<redacted>"

// scrubber replaces known values. A nil scrubber, for a run without
// credentials from the environment, leaves everything as it is.
type scrubber struct {
	needles []string
}

// newScrubber matches each value by its parts (core.SecretParts: the value,
// and the lines and JSON string fields of a key file), each as it is and
// JSON-escaped (as it appears in a JSON file or a logged JSON string); the
// longest needles are replaced first, so a part never splits a whole.
func newScrubber(values []string) *scrubber {
	seen := make(map[string]struct{})
	for _, value := range values {
		for _, part := range core.SecretParts(value) {
			seen[part] = struct{}{}
			if encoded, err := json.Marshal(part); err == nil {
				seen[string(encoded[1:len(encoded)-1])] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	needles := make([]string, 0, len(seen))
	for needle := range seen {
		needles = append(needles, needle)
	}
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	return &scrubber{needles: needles}
}

// text returns value with every needle replaced.
func (s *scrubber) text(value string) string {
	if s == nil {
		return value
	}
	for _, needle := range s.needles {
		value = strings.ReplaceAll(value, needle, redactedValue)
	}
	return value
}

// file rewrites a saved file in place when it holds a needle. A file that
// does not exist is left alone: an artifact that was never written has
// nothing to scrub.
func (s *scrubber) file(path string) error {
	if s == nil {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	scrubbed := s.text(string(content))
	if scrubbed == string(content) {
		return nil
	}
	return os.WriteFile(path, []byte(scrubbed), info.Mode().Perm())
}

// err returns an error whose message has every needle replaced; errors.Is
// and errors.As still see the original cause.
func (s *scrubber) err(err error) error {
	if s == nil || err == nil {
		return err
	}
	message := err.Error()
	scrubbed := s.text(message)
	if scrubbed == message {
		return err
	}
	return &scrubbedError{message: scrubbed, cause: err}
}

type scrubbedError struct {
	message string
	cause   error
}

func (e *scrubbedError) Error() string { return e.message }
func (e *scrubbedError) Unwrap() error { return e.cause }

// scrubber returns the scrubber for the values Tasks read from the
// environment, or nil when there are none.
func (b *Benchmark) scrubber() *scrubber {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.scrub
}
