package apptainer

import (
	"os/exec"
	"testing"
)

// os/exec copies a child's output into a non-file Stdout with io.Copy, which
// uses the writer's ReadFrom when it has one; the limit must hold on that
// path as well.
func TestBoundedBufferLimitsAChildsOutput(t *testing.T) {
	output := &boundedBuffer{limit: 4096}
	child := exec.Command("/bin/sh", "-c", "head -c 10000 /dev/zero")
	child.Stdout = output
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	if kept := len(output.Bytes()); kept != 4096 || !output.exceeded {
		t.Fatalf("kept %d of 10000 bytes, exceeded %v; want 4096 and true", kept, output.exceeded)
	}
}
