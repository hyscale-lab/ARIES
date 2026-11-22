package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSamplerReadinessAndStop(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sample.py")
	if err := os.WriteFile(script, []byte("import time\nprint('{\"pid\":1,\"pss_bytes\":12,\"end_ns\":55}', flush=True)\ntime.sleep(300)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "samples.jsonl")
	stop, err := startSampler(t.Context(), script, "unused-test-socket", out)
	if err != nil {
		t.Fatal(err)
	}
	if err = stop(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), `"pss_bytes":12`) {
		t.Fatal(string(b), err)
	}
	if err = os.WriteFile(script, []byte("print('invalid')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = startSampler(t.Context(), script, "unused", out+"-invalid"); err == nil {
		t.Fatal("accepted invalid sample")
	}
}
