package nvidia

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSourceQueriesSelectedGPUsAndParsesMetrics(t *testing.T) {
	observed := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var gotArgs []string
	source := &Source{
		taskID: "fix-git", executable: "/usr/bin/nvidia-smi", indices: []int{0, 2},
		now: func() time.Time { return observed },
		run: func(_ context.Context, executable string, args []string) ([]byte, error) {
			if executable != "/usr/bin/nvidia-smi" {
				t.Fatalf("executable = %q", executable)
			}
			gotArgs = append([]string(nil), args...)
			return []byte("0, GPU-aaa, NVIDIA H100 NVL, 87.5, 41, 12288, 95830, 312.25, 64\n2, GPU-bbb, NVIDIA H100 NVL, N/A, 0, 1024, 95830, N/A, 35\n"), nil
		},
	}
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{
		"--id=0,2",
		"--query-gpu=index,uuid,name,utilization.gpu,utilization.memory,memory.used,memory.total,power.draw,temperature.gpu",
		"--format=csv,noheader,nounits",
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %#v", gotArgs)
	}
	if len(readings) != 2 || readings[0].GPU == nil || readings[0].GPU.Index != 0 ||
		readings[0].GPU.MemoryUsageBytes != 12288<<20 || readings[0].GPU.PowerWatts == nil ||
		readings[1].GPU == nil || readings[1].GPU.Index != 2 || readings[1].GPU.UtilizationPercent != nil ||
		!readings[0].ObservedAt.Equal(observed) {
		t.Fatalf("readings = %#v", readings)
	}
}

func TestSourceRejectsMalformedOutputAndPropagatesCommandFailure(t *testing.T) {
	source := &Source{
		taskID: "fix-git", executable: "nvidia-smi", indices: []int{0}, now: time.Now,
		run: func(context.Context, string, []string) ([]byte, error) {
			return []byte("malformed\n"), nil
		},
	}
	if _, err := source.Sample(context.Background()); err == nil {
		t.Fatal("accepted malformed output")
	}
	canary := errors.New("query failed")
	source.run = func(context.Context, string, []string) ([]byte, error) { return nil, canary }
	if _, err := source.Sample(context.Background()); !errors.Is(err, canary) {
		t.Fatalf("err = %v", err)
	}
}
