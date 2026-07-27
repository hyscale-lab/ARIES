package sglang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nativeConfig = `model-path: Qwen/Qwen3-8B
served-model-name: Qwen/Qwen3-8B
host: 0.0.0.0
port: 30000
device: cuda
tensor-parallel-size: 1
context-length: 32768
mem-fraction-static: 0.85
reasoning-parser: qwen3
tool-call-parser: qwen
`

func TestSGLangNativeConfigStrictAndCrossChecked(t *testing.T) {
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "native.yaml")
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := LoadNativeConfig(write(nativeConfig), "Qwen/Qwen3-8B", "http://host:30000/v1")
	if err != nil || cfg.Port != 30000 {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	invalid := []string{nativeConfig + "future: true\n", nativeConfig + "---\nhost: other\n", strings.Replace(nativeConfig, "port: 30000", "port: 0", 1), strings.Replace(nativeConfig, "served-model-name: Qwen/Qwen3-8B", "served-model-name: other", 1)}
	for _, content := range invalid {
		if _, err := LoadNativeConfig(write(content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil {
			t.Fatal("expected rejection")
		}
	}
	if _, err := LoadNativeConfig(write(nativeConfig), "Qwen/Qwen3-8B", "http://host/v1"); err == nil {
		t.Fatal("accepted implicit port")
	}
}
