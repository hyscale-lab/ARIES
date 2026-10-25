// Package microbench checks the pin of the packed microbenchmark tasks.
package microbench

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func readVersions(t *testing.T, path string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var versions map[string]any
	if err := json.Unmarshal(content, &versions); err != nil {
		t.Fatal(err)
	}
	return versions
}

// The microbenchmark versions file differs from the main one only in the
// Terminal-Bench pin, so the other images and datasets cannot drift apart.
func TestMicrobenchVersionsDifferOnlyInTheTaskPin(t *testing.T) {
	main := readVersions(t, "../../configs/versions.json")
	micro := readVersions(t, "../../configs/microbench-versions.json")
	delete(main, "terminalbench2")
	delete(micro, "terminalbench2")
	if !reflect.DeepEqual(main, micro) {
		t.Fatal("configs/microbench-versions.json differs from configs/versions.json outside terminalbench2; copy the change across")
	}
}

// The pin names exactly the committed tasks, so a task edit cannot run under
// an old pin.
func TestMicrobenchPinMatchesTheCommittedTasks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.Command("git", "-C", "../..", "rev-parse", "HEAD").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	output, err := exec.Command("bash", "pack.sh", "--print-revision").CombinedOutput()
	if err != nil {
		t.Fatalf("pack.sh: %v: %s", err, output)
	}
	packed := strings.TrimSpace(string(output))
	pin := readVersions(t, "../../configs/microbench-versions.json")["terminalbench2"].(map[string]any)["revision"]
	if packed != pin {
		t.Fatalf("the committed tasks pack to %s but configs/microbench-versions.json pins %v; update the pin", packed, pin)
	}
}
