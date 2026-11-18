package apptainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRunLeftoversSelectOnlyTheRunsRecords(t *testing.T) {
	// A glob character in the path must not hide the records.
	state := filepath.Join(t.TempDir(), "exp[1]")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	managed := func(run string) map[string]string {
		return map[string]string{"aries.managed": "true", "aries.run": run}
	}
	for directory, current := range map[string]record{
		"hermes-a":   {Name: "hermes-a", Labels: managed("run-1")},
		"sandbox-a":  {Name: "sandbox-a", Labels: managed("run-1")},
		"sandbox-b":  {Name: "sandbox-b", Labels: managed("run-2")},
		"unmanaged":  {Name: "unmanaged", Labels: map[string]string{"aries.run": "run-1"}},
		"misplaced":  {Name: "elsewhere", Labels: managed("run-1")},
		"no-labels":  {Name: "no-labels"},
		"bridge-run": {},
	} {
		if err := os.Mkdir(filepath.Join(state, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		if directory == "bridge-run" {
			continue // a bridge's workdir holds no instance record
		}
		raw, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, directory, recordFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, run := range map[string]string{"bridge-run-1": "run-1", "bridge-run-2": "run-2"} {
		labels := managed(run)
		labels["aries.component"] = "bridge"
		raw, err := json.Marshal(processFile{Name: name, Labels: labels, Workdir: filepath.Join(state, name)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(processRecordPath(filepath.Join(state, name)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	instances, err := RunInstances(state, "run-1")
	if err != nil || !reflect.DeepEqual(instances, []string{"hermes-a", "sandbox-a"}) {
		t.Fatalf("instances=%v err=%v", instances, err)
	}
	processes, err := RunProcesses(state, "run-1")
	if err != nil || !reflect.DeepEqual(processes, []string{"bridge-run-1"}) {
		t.Fatalf("processes=%v err=%v", processes, err)
	}
}

func TestRunLeftoversOfAMissingStateDirectoryAreNone(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	instances, err := RunInstances(missing, "run-1")
	if err != nil || len(instances) != 0 {
		t.Fatalf("instances=%v err=%v", instances, err)
	}
	processes, err := RunProcesses(missing, "run-1")
	if err != nil || len(processes) != 0 {
		t.Fatalf("processes=%v err=%v", processes, err)
	}
}

func TestRunLeftoversFailOnARecordTheyCannotRead(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "broken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "broken", recordFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunInstances(state, "run-1"); err == nil {
		t.Fatal("an unreadable record could be this run's; it must not be skipped")
	}
	if err := os.WriteFile(filepath.Join(state, "bridge-x"+processRecordSuffix), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunProcesses(state, "run-1"); err == nil {
		t.Fatal("an unreadable process record could be this run's; it must not be skipped")
	}
}
