package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Components consume the same neutral deployment contract. Keeping provider
// imports here would make adding a provider require rewriting each component.
func TestComponentsUseNeutralDeploymentBoundary(t *testing.T) {
	for _, component := range []string{"harness", "sandbox", "bridge"} {
		root := filepath.Join("..", "..", "pkg", component)
		for path, imports := range productionImports(t, root, true) {
			for _, imported := range imports {
				if providerImport(imported) {
					t.Errorf("%s imports provider implementation %s", path, imported)
				}
			}
		}
	}
}

func productionImports(t *testing.T, root string, recursive bool) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if !recursive && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		result[path] = nil
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			result[path] = append(result[path], imported)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Fatalf("no production files inspected in %s", root)
	}
	return result
}

func TestBridgeLifecycleAndEngineDoNotImportDialects(t *testing.T) {
	for _, component := range []string{"pkg/bridge", "pkg/bridge/ssh"} {
		root := filepath.Join("..", "..", filepath.FromSlash(component))
		for path, imports := range productionImports(t, root, false) {
			for _, imported := range imports {
				if dialectImport(imported) {
					t.Errorf("%s imports concrete dialect %s; supply policy through wiring", path, imported)
				}
			}
		}
	}
}

func TestOpenClawClientDoesNotImportServerDialects(t *testing.T) {
	for _, component := range []string{"pkg/bridge/ssh/openclaw/client", "cmd/aries-ssh-client"} {
		root := filepath.Join("..", "..", filepath.FromSlash(component))
		for path, imports := range productionImports(t, root, true) {
			for _, imported := range imports {
				if dialectImport(imported) && !openclawClientImport(imported) {
					t.Errorf("%s imports server dialect %s; the client must forward commands unchanged", path, imported)
				}
			}
		}
	}
}

func openclawClientImport(path string) bool {
	const prefix = "github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client"
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func dialectImport(path string) bool {
	for _, dialect := range []string{"openclaw", "hermes"} {
		prefix := "github.com/hyscale-lab/aries/pkg/bridge/ssh/" + dialect
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func TestBridgeBoundaryRecognizesDialectImports(t *testing.T) {
	for _, path := range []string{
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/internal/grammar",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client",
	} {
		if !dialectImport(path) {
			t.Fatalf("concrete dialect import accepted: %s", path)
		}
	}
	for _, path := range []string{
		"github.com/hyscale-lab/aries/pkg/bridge",
		"github.com/hyscale-lab/aries/pkg/bridge/target",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials",
	} {
		if dialectImport(path) {
			t.Fatalf("shared contract/helper rejected: %s", path)
		}
	}
}

func TestOpenClawClientImportExceptionExcludesServerGrammar(t *testing.T) {
	for _, path := range []string{
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client/internal/helper",
	} {
		if !openclawClientImport(path) {
			t.Fatalf("client adapter rejected: %s", path)
		}
	}
	for _, path := range []string{
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/internal/grammar",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes",
		"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/clientgrammar",
	} {
		if !dialectImport(path) || openclawClientImport(path) {
			t.Fatalf("server dialect accepted as client adapter: %s", path)
		}
	}
}

func providerImport(path string) bool {
	return strings.HasPrefix(path, "github.com/moby/") || strings.HasPrefix(path, "github.com/containerd/") || strings.HasPrefix(path, "github.com/hyscale-lab/aries/pkg/deployment/")
}

func TestDeploymentBoundaryRecognizesProviderImports(t *testing.T) {
	for _, path := range []string{"github.com/moby/moby/client", "github.com/containerd/errdefs", "github.com/hyscale-lab/aries/pkg/deployment/docker"} {
		if !providerImport(path) {
			t.Fatalf("provider import accepted: %s", path)
		}
	}
	if providerImport("github.com/hyscale-lab/aries/pkg/deployment") {
		t.Fatal("neutral contract rejected")
	}
}

func TestCommandUsesWiringForConcreteConstruction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, prefix := range []string{
				"github.com/hyscale-lab/aries/pkg/benchmark/",
				"github.com/hyscale-lab/aries/pkg/harness/",
				"github.com/hyscale-lab/aries/pkg/bridge/",
				"github.com/hyscale-lab/aries/pkg/sandbox",
				"github.com/hyscale-lab/aries/internal/modelruntime/",
			} {
				if strings.HasPrefix(imported, prefix) {
					t.Errorf("%s imports concrete implementation %s; use internal/app/wiring", path, imported)
				}
			}
			if providerImport(imported) {
				t.Errorf("%s imports deployment provider %s; use internal/app/wiring", path, imported)
			}
		}
	}
}
