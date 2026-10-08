//go:build integration

package dockerroute

import (
	"context"
	"testing"
	"time"

	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
)

// Environment owns connectivity for a test run. Its provider stays open until
// run cleanup, independently of any sandbox manager's deployment client.
func Environment(t *testing.T, runID string) *dockerdeployment.RunEnvironment {
	t.Helper()
	provider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	environment := provider.NewRunEnvironment(runID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := environment.Stop(ctx); err != nil {
			t.Errorf("run environment cleanup: %v", err)
		}
		if err := provider.Close(); err != nil {
			t.Errorf("run environment transport cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := environment.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return environment
}
