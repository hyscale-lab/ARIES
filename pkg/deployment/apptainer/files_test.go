package apptainer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTypedFileOperationsAreUnsupported(t *testing.T) {
	manager := &Manager{}
	ctx := context.Background()
	_, stat := manager.StatFile(ctx, "id", "/app/f")
	_, _, open := manager.OpenFile(ctx, "id", "/app/f")
	_, write := manager.WriteFile(ctx, "id", "/app/f", strings.NewReader("x"), 1, "")
	for name, err := range map[string]error{"stat": stat, "open": open, "write": write} {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("%s error = %v", name, err)
		}
	}
}
