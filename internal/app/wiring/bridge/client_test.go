package bridge

import (
	"reflect"
	"testing"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
)

func TestSelectedBenchmarkRevisionsExcludeUnselectedPins(t *testing.T) {
	pins := config.Versions{
		TerminalBench2:    config.TerminalBench2Versions{Revision: "terminal-pin"},
		DeepResearchBench: config.DeepResearchBenchVersions{Revision: "research-pin"},
		SWEAtlas:          config.SWEAtlasVersions{Revision: "atlas-pin"},
		SWEbenchPro:       config.SWEbenchProVersions{DatasetRevision: "dataset-pin", EvaluatorRevision: "evaluator-pin"},
		Toolathlon:        config.ToolathlonVersions{Revision: "excluded-pin"},
	}
	for _, test := range []struct {
		name string
		want map[string]string
	}{
		{name: "terminalbench2", want: map[string]string{"revision": "terminal-pin"}},
		{name: "deepresearchbench", want: map[string]string{"revision": "research-pin"}},
		{name: "sweatlasqa", want: map[string]string{"revision": "atlas-pin"}},
		{name: "swebenchpro", want: map[string]string{"dataset_revision": "dataset-pin", "evaluator_revision": "evaluator-pin"}},
		{name: "toolathlon"},
	} {
		if got := benchmarkRevisions(config.Config{Benchmark: config.BenchmarkConfig{Type: test.name}, Versions: pins}); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("selected %s pins = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestClientConfigSelectsTheHelperOfTheBridgeType(t *testing.T) {
	const helperDir = "/opt/build/bin"
	for bridgeType, want := range map[string]managed.ClientConfig{
		"hermes-ssh":   {},
		"openclaw-ssh": {Command: "/opt/aries/bin/aries-ssh-client", SourcePath: "/opt/build/bin/aries-ssh-client"},
		"hermes-grpc":  {},
	} {
		got, err := ClientConfig(bridgeType, helperDir)
		if err != nil || got != want {
			t.Fatalf("ClientConfig(%q) = %#v, %v; want %#v", bridgeType, got, err, want)
		}
	}
	for _, bridgeType := range []string{"openclaw-ssh"} {
		if _, err := ClientConfig(bridgeType, ""); err == nil {
			t.Fatalf("ClientConfig(%q) accepted a missing helper directory", bridgeType)
		}
	}
	if _, err := ClientConfig("other", helperDir); err == nil {
		t.Fatal("unknown bridge type accepted")
	}
}
