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
func TestHarnessAndSandboxUseNeutralDeploymentBoundary(t *testing.T) {
	for _, component := range []string{"harness", "sandbox"} {
		count := 0
		root := filepath.Join("..", "..", "pkg", component)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			count++
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range parsed.Imports {
				imported, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if providerImport(imported) {
					t.Errorf("%s imports provider implementation %s", path, imported)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("no production files inspected for %s", component)
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
