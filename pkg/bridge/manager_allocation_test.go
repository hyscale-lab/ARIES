package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

type allocationRuntime struct {
	*launchRuntime
	id  string
	err error
}

func (r *allocationRuntime) Create(_ context.Context, request deployment.Request) (string, error) {
	r.request = request
	return r.id, r.err
}

func TestManagerPreservesUnconfirmedAllocationInCleanup(t *testing.T) {
	for _, test := range []struct {
		name, id  string
		err       error
		uncertain bool
	}{
		{"confirmed-absent", "", errors.New("create rejected"), false},
		{"owned-partial-create", "service-fixture-123", errors.New("create response lost, identity recovered"), false},
		{"unknown-allocation", "", errors.Join(errors.New("daemon unavailable"), deployment.ErrAllocationUnconfirmed), true},
		{"invalid-empty-success", "", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, runtime, sandbox := launchFixture(t, "docker")
			m.options.Runtime = &allocationRuntime{launchRuntime: runtime, id: test.id, err: test.err}
			if _, err := m.Start(context.Background(), sandbox); err == nil {
				t.Fatal("missing startup failure")
			}
			for range 2 {
				err := m.Stop(context.Background())
				if errors.Is(err, deployment.ErrAllocationUnconfirmed) != test.uncertain || !test.uncertain && err != nil {
					t.Fatalf("cleanup lost allocation status: %v", err)
				}
			}
			contents, err := os.ReadFile(filepath.Join(m.local, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct{ RuntimeName string }
			if err = json.Unmarshal(contents, &record); err != nil || record.RuntimeName != runtime.request.Name || record.RuntimeName == "" {
				t.Fatalf("allocation ownership not recorded: %+v %v", record, err)
			}
		})
	}
}

func TestManagerUsesInjectedClientPolicyForAnotherSSHHarness(t *testing.T) {
	m, _, sandbox := launchFixture(t, "docker")
	source := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(source, []byte("fixture-helper"), 0755); err != nil {
		t.Fatal(err)
	}
	m.options.Client = ClientConfig{IdentityFile: "/fixture/key", KnownHostsFile: "/fixture/hosts", Command: "/fixture/bin/forwarder", SourcePath: source}
	endpoint, err := m.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.IdentityFile != "/fixture/key" || endpoint.KnownHostsFile != "/fixture/hosts" || endpoint.ClientCommand != "/fixture/bin/forwarder" {
		t.Fatalf("injected client policy replaced: %+v", endpoint)
	}
	contents, err := os.ReadFile(endpoint.ClientSourceFile)
	if err != nil || string(contents) != "fixture-helper" {
		t.Fatalf("helper staging: %v", err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.ClientSourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged helper survived cleanup")
	}
	if _, err := os.Stat(endpoint.KnownHostsSourceFile); err != nil {
		t.Fatal("public host identity evidence lost", err)
	}
}
