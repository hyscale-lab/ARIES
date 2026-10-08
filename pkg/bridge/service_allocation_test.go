package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"os"
	"path/filepath"
	"testing"
)

type allocationRuntime struct {
	*launchRuntime
	id  string
	err error
}

func (r *allocationRuntime) Create(_ context.Context, req deployment.Request) (string, error) {
	r.request = req
	return r.id, r.err
}
func TestServicePreservesUnconfirmedAllocation(t *testing.T) {
	for _, test := range []struct {
		name, id  string
		err       error
		uncertain bool
	}{{"absent", "", errors.New("create rejected"), false}, {"partial", "service-fixture-123", errors.New("response lost"), false}, {"uncertain", "", deployment.ErrAllocationUnconfirmed, true}, {"empty-success", "", nil, true}} {
		t.Run(test.name, func(t *testing.T) {
			s, r, _ := launchFixture(t, "docker")
			s.options.Runtime = &allocationRuntime{r, test.id, test.err}
			if err := s.Start(context.Background()); err == nil {
				t.Fatal("startup success")
			}
			for range 2 {
				err := s.Stop(context.Background())
				if errors.Is(err, deployment.ErrAllocationUnconfirmed) != test.uncertain || !test.uncertain && err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(filepath.Join(s.options.OutputDir, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record struct{ RuntimeName string }
			if json.Unmarshal(data, &record) != nil || record.RuntimeName == "" || record.RuntimeName != r.request.Name {
				t.Fatal(string(data))
			}
		})
	}
}
