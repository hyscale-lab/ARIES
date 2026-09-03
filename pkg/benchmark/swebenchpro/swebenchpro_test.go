package swebenchpro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/parquet-go/parquet-go"
)

const (
	fixtureInstanceID = "django__django-12345"
	fixtureGoldPatch  = "GOLD_SOLUTION_MUST_STAY_PRIVATE"
	fixtureTestPatch  = "PRIVATE_TEST_PATCH_MUST_STAY_PRIVATE"
)

func TestParsePythonStringList(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "empty", input: "[]", want: []string{}},
		{name: "mixed quotes", input: `["one", 'two', "three, four"]`, want: []string{"one", "two", "three, four"}},
		{name: "escaped", input: `['can\'t', "quoted \"test\"", 'path\\name']`, want: []string{"can't", `quoted "test"`, `path\name`}},
		{name: "trailing comma", input: "[ 'one', ]", want: []string{"one"}},
		{name: "not list", input: `'one'`, wantErr: true},
		{name: "non string", input: `['one', 2]`, wantErr: true},
		{name: "call", input: `[open('/secret')]`, wantErr: true},
		{name: "concatenation", input: `['one' + 'two']`, wantErr: true},
		{name: "trailing token", input: `['one'] garbage`, wantErr: true},
		{name: "unterminated", input: `['one]`, wantErr: true},
		{name: "newline", input: "['one\\nline']", wantErr: true},
		{name: "nul", input: "['one\\x00two']", wantErr: true},
		{name: "duplicate", input: `['one', 'one']`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePythonStringList(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parsePythonStringList(%q) accepted", test.input)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parsePythonStringList(%q) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}

func TestLoadDatasetMapsTaskAndKeepsGoldPrivate(t *testing.T) {
	datasetRoot, evaluatorRoot := writeDatasetFixture(t, []datasetRow{fixtureRow()})
	records, err := loadDataset(datasetRoot, evaluatorRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	task, details, err := records[0].task(fixtureInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	wantInstruction := "Fix the reported regression.\n\nRequirements:\nKeep compatibility.\n\nNew interfaces introduced:\nNo public API changes."
	if task.ID != fixtureInstanceID || task.Instruction != wantInstruction {
		t.Fatalf("task = %#v", task)
	}
	if task.Environment.Image != "docker.io/jefzda/sweap-images:django__django-12345" || task.Environment.Workdir != repositoryPath || task.Environment.ExecUser != agentExecUser || task.Timeout != defaultAgentTimeout {
		t.Fatalf("environment = %#v timeout=%s", task.Environment, task.Timeout)
	}
	if !task.Environment.AllowNetwork {
		t.Fatal("task sandbox network is disabled, so the harness cannot reach its model endpoint")
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), agentExecUser) || strings.Contains(string(encoded), "ExecUser") {
		t.Fatalf("task JSON exposes internal agent identity: %s", encoded)
	}
	for _, private := range []string{fixtureGoldPatch, fixtureTestPatch, "git checkout", "tests/regression_test.py", "run script sentinel", "parser sentinel"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("task JSON exposes private value %q: %s", private, encoded)
		}
	}
	if strings.Contains(details.testPatch, fixtureGoldPatch) || details.testPatch != fixtureTestPatch {
		t.Fatalf("private details retained the wrong patch: %#v", details)
	}
	if details.baseCommit != strings.Repeat("a", 40) || !reflect.DeepEqual(details.failToPass, []string{"tests.regression_test::test_fix"}) || !reflect.DeepEqual(details.passToPass, []string{"tests.existing_test::test_ok"}) || !reflect.DeepEqual(details.selectedTests, []string{"tests/regression_test.py"}) {
		t.Fatalf("private details = %#v", details)
	}
	if !reflect.DeepEqual(details.verifierFiles, []string{"tests/regression_test.py"}) {
		t.Fatalf("private verifier files = %#v", details.verifierFiles)
	}
}

func TestLoadDatasetSeparatesRunSelectorsFromVerifierFiles(t *testing.T) {
	row := fixtureRow()
	row.SelectedTestFilesToRun = `['TestParseResourcePath//api/v1/nodes/foo']`
	datasetRoot, evaluatorRoot := writeDatasetFixture(t, []datasetRow{row})
	records, err := loadDataset(datasetRoot, evaluatorRoot)
	if err != nil {
		t.Fatal(err)
	}
	details := records[0].details
	if !reflect.DeepEqual(details.selectedTests, []string{"TestParseResourcePath//api/v1/nodes/foo"}) {
		t.Fatalf("run-script selectors = %#v", details.selectedTests)
	}
	if !reflect.DeepEqual(details.verifierFiles, []string{"tests/regression_test.py"}) {
		t.Fatalf("private verifier files = %#v", details.verifierFiles)
	}
}

func TestLoadDatasetRequiresExactSchema(t *testing.T) {
	t.Run("missing column", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, datasetParquetPath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		type missingDockerTag struct {
			Repo       string `parquet:"repo"`
			InstanceID string `parquet:"instance_id"`
		}
		if err := parquet.WriteFile(path, []missingDockerTag{{Repo: "django/django", InstanceID: fixtureInstanceID}}); err != nil {
			t.Fatal(err)
		}
		if _, err := loadDataset(root, t.TempDir()); err == nil || !strings.Contains(err.Error(), "schema") {
			t.Fatalf("loadDataset error = %v, want schema rejection", err)
		}
	})

	t.Run("duplicate instance", func(t *testing.T) {
		root, evaluator := writeDatasetFixture(t, []datasetRow{fixtureRow(), fixtureRow()})
		if _, err := loadDataset(root, evaluator); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("loadDataset error = %v, want duplicate rejection", err)
		}
	})
}

func TestDatasetRowValidationRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		change func(*datasetRow)
	}{
		{name: "unsafe ID", change: func(row *datasetRow) { row.InstanceID = "bad/id" }},
		{name: "empty statement", change: func(row *datasetRow) { row.ProblemStatement = " " }},
		{name: "bad base commit", change: func(row *datasetRow) { row.BaseCommit = "main" }},
		{name: "bad image tag", change: func(row *datasetRow) { row.DockerHubTag = "bad tag" }},
		{name: "empty selected tests", change: func(row *datasetRow) { row.SelectedTestFilesToRun = "[]" }},
		{name: "empty fail to pass", change: func(row *datasetRow) { row.FailToPass = "[]" }},
		{name: "malformed pass to pass", change: func(row *datasetRow) { row.PassToPass = "not-a-list" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := fixtureRow()
			test.change(&row)
			root, evaluator := writeDatasetFixture(t, []datasetRow{row})
			if _, err := loadDataset(root, evaluator); err == nil {
				t.Fatal("invalid dataset row accepted")
			}
		})
	}
}

func fixtureRow() datasetRow {
	return datasetRow{
		Repo:                   "django/django",
		InstanceID:             fixtureInstanceID,
		BaseCommit:             strings.Repeat("a", 40),
		Patch:                  fixtureGoldPatch,
		TestPatch:              fixtureTestPatch,
		ProblemStatement:       "Fix the reported regression.",
		Requirements:           "Keep compatibility.",
		Interface:              "No public API changes.",
		RepoLanguage:           "python",
		FailToPass:             `['tests.regression_test::test_fix']`,
		PassToPass:             `["tests.existing_test::test_ok"]`,
		IssueSpecificity:       "high",
		IssueCategories:        "bug",
		BeforeRepoSetCommand:   "git reset --hard aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\ngit clean -fd\ngit checkout aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\ngit checkout bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb -- tests/regression_test.py",
		SelectedTestFilesToRun: `['tests/regression_test.py']`,
		DockerHubTag:           "django__django-12345",
	}
}

func writeDatasetFixture(t *testing.T, rows []datasetRow) (string, string) {
	t.Helper()
	datasetRoot := t.TempDir()
	parquetPath := filepath.Join(datasetRoot, datasetParquetPath)
	if err := os.MkdirAll(filepath.Dir(parquetPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(parquetPath, rows); err != nil {
		t.Fatal(err)
	}
	evaluatorRoot := t.TempDir()
	writeTestFile(t, filepath.Join(evaluatorRoot, runScriptsDirectory, fixtureInstanceID, runScriptName), "#!/bin/bash\n# run script sentinel\n")
	writeTestFile(t, filepath.Join(evaluatorRoot, runScriptsDirectory, fixtureInstanceID, parserFileName), "# parser sentinel\n")
	return datasetRoot, evaluatorRoot
}

func writeTestFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
