package swebenchpro

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/parquet-go/parquet-go"
)

const (
	datasetParquetPath = "data/test-00000-of-00001.parquet"
	publicTaskCount    = 731
	maxDatasetSize     = 16 << 20
	maxDatasetText     = 64 << 20
	maxDatasetRows     = 1000
)

var datasetColumns = []string{
	"repo",
	"instance_id",
	"base_commit",
	"patch",
	"test_patch",
	"problem_statement",
	"requirements",
	"interface",
	"repo_language",
	"fail_to_pass",
	"pass_to_pass",
	"issue_specificity",
	"issue_categories",
	"before_repo_set_cmd",
	"selected_test_files_to_run",
	"dockerhub_tag",
}

type datasetRow struct {
	Repo                   string `parquet:"repo"`
	InstanceID             string `parquet:"instance_id"`
	BaseCommit             string `parquet:"base_commit"`
	Patch                  string `parquet:"patch"`
	TestPatch              string `parquet:"test_patch"`
	ProblemStatement       string `parquet:"problem_statement"`
	Requirements           string `parquet:"requirements"`
	Interface              string `parquet:"interface"`
	RepoLanguage           string `parquet:"repo_language"`
	FailToPass             string `parquet:"fail_to_pass"`
	PassToPass             string `parquet:"pass_to_pass"`
	IssueSpecificity       string `parquet:"issue_specificity"`
	IssueCategories        string `parquet:"issue_categories"`
	BeforeRepoSetCommand   string `parquet:"before_repo_set_cmd"`
	SelectedTestFilesToRun string `parquet:"selected_test_files_to_run"`
	DockerHubTag           string `parquet:"dockerhub_tag"`
}

type taskRecord struct {
	row     datasetRow
	details taskDetails
}

func loadDataset(datasetRoot, evaluatorRoot string) ([]taskRecord, error) {
	filePath := filepath.Join(datasetRoot, datasetParquetPath)
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open SWE-bench Pro dataset: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat SWE-bench Pro dataset: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxDatasetSize {
		return nil, fmt.Errorf("SWE-bench Pro dataset must be a nonempty regular parquet file no larger than %d bytes", maxDatasetSize)
	}
	parquetFile, err := parquet.OpenFile(file, info.Size())
	if err != nil {
		return nil, fmt.Errorf("open SWE-bench Pro parquet: %w", err)
	}
	if err := validateDatasetSchema(parquetFile); err != nil {
		return nil, err
	}
	if parquetFile.NumRows() <= 0 || parquetFile.NumRows() > maxDatasetRows {
		return nil, fmt.Errorf("SWE-bench Pro parquet declares unsafe row count %d", parquetFile.NumRows())
	}

	reader := parquet.NewReader(parquetFile)
	defer reader.Close()
	records := make([]taskRecord, 0, int(parquetFile.NumRows()))
	seen := make(map[string]struct{}, parquetFile.NumRows())
	totalText := 0
	for index := int64(0); ; index++ {
		var row datasetRow
		err := reader.Read(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode SWE-bench Pro parquet row %d: %w", index, err)
		}
		for _, value := range row.strings() {
			totalText += len(value)
			if totalText > maxDatasetText {
				return nil, fmt.Errorf("SWE-bench Pro decoded text exceeds %d bytes", maxDatasetText)
			}
		}
		details, err := row.validate(evaluatorRoot)
		if err != nil {
			return nil, fmt.Errorf("validate SWE-bench Pro row %d instance %q: %w", index, row.InstanceID, err)
		}
		if _, duplicate := seen[row.InstanceID]; duplicate {
			return nil, fmt.Errorf("duplicate SWE-bench Pro instance ID %q", row.InstanceID)
		}
		seen[row.InstanceID] = struct{}{}
		records = append(records, taskRecord{row: row, details: details})
	}
	if int64(len(records)) != parquetFile.NumRows() {
		return nil, fmt.Errorf("SWE-bench Pro parquet decoded %d rows; metadata declares %d", len(records), parquetFile.NumRows())
	}
	return records, nil
}

func validateDatasetSchema(file *parquet.File) error {
	columns := file.Schema().Columns()
	got := make([]string, len(columns))
	for index, column := range columns {
		if len(column) != 1 {
			return fmt.Errorf("SWE-bench Pro parquet schema column %d is nested: %v", index, column)
		}
		got[index] = column[0]
		leaf, ok := file.Schema().Lookup(column...)
		if !ok || leaf.Node.Type().String() != parquet.String().Type().String() {
			return fmt.Errorf("SWE-bench Pro parquet schema column %q must be a string", column[0])
		}
	}
	if !reflect.DeepEqual(got, datasetColumns) {
		return fmt.Errorf("SWE-bench Pro parquet schema columns = %v, want %v", got, datasetColumns)
	}
	return nil
}

func (row datasetRow) validate(evaluatorRoot string) (taskDetails, error) {
	for index, value := range row.strings() {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return taskDetails{}, fmt.Errorf("field %q is not valid NUL-free UTF-8", datasetColumns[index])
		}
	}
	if !safeTaskID(row.InstanceID) {
		return taskDetails{}, errors.New("instance_id is unsafe")
	}
	if strings.TrimSpace(row.ProblemStatement) == "" {
		return taskDetails{}, errors.New("problem_statement is empty")
	}
	if !isHex(row.BaseCommit, 40) {
		return taskDetails{}, errors.New("base_commit must be a 40-character Git revision")
	}
	if strings.TrimSpace(row.TestPatch) == "" {
		return taskDetails{}, errors.New("test_patch is empty")
	}
	if strings.TrimSpace(row.Patch) == "" {
		return taskDetails{}, errors.New("gold patch is empty")
	}
	if _, err := containerimage.ValidateTagOnly("docker.io/jefzda/sweap-images:" + row.DockerHubTag); err != nil {
		return taskDetails{}, fmt.Errorf("dockerhub_tag: %w", err)
	}
	failToPass, err := parsePythonStringList(row.FailToPass)
	if err != nil || len(failToPass) == 0 {
		return taskDetails{}, fmt.Errorf("fail_to_pass must be a nonempty string list: %w", err)
	}
	passToPass, err := parsePythonStringList(row.PassToPass)
	if err != nil {
		return taskDetails{}, fmt.Errorf("pass_to_pass must be a string list: %w", err)
	}
	selectedTests, err := parsePythonStringList(row.SelectedTestFilesToRun)
	if err != nil || len(selectedTests) == 0 {
		return taskDetails{}, fmt.Errorf("selected_test_files_to_run must be a nonempty string list: %w", err)
	}
	goldCommit, checkoutPaths, err := parseBeforeRepoSetCommand(row.BeforeRepoSetCommand, row.BaseCommit)
	if err != nil {
		return taskDetails{}, err
	}

	script := filepath.Join(evaluatorRoot, runScriptsDirectory, row.InstanceID, runScriptName)
	parser := filepath.Join(evaluatorRoot, runScriptsDirectory, row.InstanceID, parserFileName)
	for name, source := range map[string]string{"run script": script, "parser": parser} {
		info, err := os.Lstat(source)
		if err != nil {
			return taskDetails{}, fmt.Errorf("inspect evaluator %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return taskDetails{}, fmt.Errorf("evaluator %s is not a regular file", name)
		}
	}
	return taskDetails{
		baseCommit:    row.BaseCommit,
		goldCommit:    goldCommit,
		testPatch:     row.TestPatch,
		failToPass:    failToPass,
		passToPass:    passToPass,
		selectedTests: selectedTests,
		verifierFiles: checkoutPaths,
		runScript:     script,
		parser:        parser,
	}, nil
}

func (row datasetRow) strings() []string {
	return []string{
		row.Repo, row.InstanceID, row.BaseCommit, row.Patch, row.TestPatch,
		row.ProblemStatement, row.Requirements, row.Interface, row.RepoLanguage,
		row.FailToPass, row.PassToPass, row.IssueSpecificity, row.IssueCategories,
		row.BeforeRepoSetCommand, row.SelectedTestFilesToRun, row.DockerHubTag,
	}
}

func parsePythonStringList(input string) ([]string, error) {
	parser := pythonListParser{input: input}
	return parser.parse()
}

type pythonListParser struct {
	input string
	index int
}

func (p *pythonListParser) parse() ([]string, error) {
	p.space()
	if !p.take('[') {
		return nil, errors.New("expected '['")
	}
	p.space()
	if p.take(']') {
		p.space()
		if p.index != len(p.input) {
			return nil, errors.New("unexpected trailing content")
		}
		return []string{}, nil
	}
	values := make([]string, 0)
	seen := make(map[string]struct{})
	for {
		p.space()
		value, err := p.string()
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("duplicate string %q", value)
		}
		seen[value] = struct{}{}
		values = append(values, value)
		p.space()
		if p.take(']') {
			break
		}
		if !p.take(',') {
			return nil, errors.New("expected ',' or ']'")
		}
		p.space()
		if p.take(']') {
			break
		}
	}
	p.space()
	if p.index != len(p.input) {
		return nil, errors.New("unexpected trailing content")
	}
	return values, nil
}

func (p *pythonListParser) string() (string, error) {
	if p.index >= len(p.input) || p.input[p.index] != '\'' && p.input[p.index] != '"' {
		return "", errors.New("expected quoted string")
	}
	quote := p.input[p.index]
	p.index++
	var value strings.Builder
	for p.index < len(p.input) {
		character := p.input[p.index]
		p.index++
		if character == quote {
			result := value.String()
			if result == "" || !utf8.ValidString(result) || strings.IndexFunc(result, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return "", errors.New("list strings must be nonempty control-free UTF-8")
			}
			return result, nil
		}
		if character != '\\' {
			if character < 0x20 || character == 0x7f {
				return "", errors.New("unescaped control character in string")
			}
			value.WriteByte(character)
			continue
		}
		if p.index >= len(p.input) {
			return "", errors.New("unterminated escape")
		}
		escaped := p.input[p.index]
		p.index++
		switch escaped {
		case '\\', '\'', '"', '/':
			value.WriteByte(escaped)
		case 'x':
			r, err := p.hexRune(2)
			if err != nil {
				return "", err
			}
			value.WriteRune(r)
		case 'u':
			r, err := p.hexRune(4)
			if err != nil {
				return "", err
			}
			value.WriteRune(r)
		case 'U':
			r, err := p.hexRune(8)
			if err != nil {
				return "", err
			}
			value.WriteRune(r)
		case 'a', 'b', 'f', 'n', 'r', 't', 'v':
			return "", errors.New("control-character escape is not allowed")
		default:
			return "", fmt.Errorf("unsupported escape \\%c", escaped)
		}
	}
	return "", errors.New("unterminated string")
}

func (p *pythonListParser) hexRune(digits int) (rune, error) {
	if p.index+digits > len(p.input) {
		return 0, errors.New("short hexadecimal escape")
	}
	value, err := strconv.ParseUint(p.input[p.index:p.index+digits], 16, 32)
	if err != nil {
		return 0, errors.New("invalid hexadecimal escape")
	}
	p.index += digits
	r := rune(value)
	if !utf8.ValidRune(r) || r < 0x20 || r == 0x7f {
		return 0, errors.New("escaped rune is not allowed")
	}
	return r, nil
}

func (p *pythonListParser) space() {
	for p.index < len(p.input) {
		switch p.input[p.index] {
		case ' ', '\t', '\r', '\n':
			p.index++
		default:
			return
		}
	}
}

func (p *pythonListParser) take(character byte) bool {
	if p.index < len(p.input) && p.input[p.index] == character {
		p.index++
		return true
	}
	return false
}
