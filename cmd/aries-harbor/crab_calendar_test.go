package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCrabRejectsBeforeGuestAccess(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(trace, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ arm, run, profile, want string }{
		{"checkpoint", "../existing", "off", "new -crab-run"},
		{"checkpoint", "fresh", "off", "source bytes do not match"},
		{"control", "fresh", "unsupported", "new -crab-run"},
	} {
		err := crabCalendar(context.Background(), trace, tc.arm, tc.run, tc.profile)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}
