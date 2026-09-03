package swebenchpro

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

func parseBeforeRepoSetCommand(command, baseCommit string) (string, []string, error) {
	lines := strings.Split(strings.TrimSpace(command), "\n")
	if len(lines) != 4 {
		return "", nil, errors.New("before_repo_set_cmd must contain the pinned four-command setup")
	}
	want := [][]string{
		{"git", "reset", "--hard", baseCommit},
		{"git", "clean", "-fd"},
		{"git", "checkout", baseCommit},
	}
	for index := range want {
		words, err := parseShellWords(lines[index])
		if err != nil || !slicesEqual(words, want[index]) {
			return "", nil, fmt.Errorf("before_repo_set_cmd line %d does not match the pinned setup contract", index+1)
		}
	}
	words, err := parseShellWords(lines[3])
	if err != nil || len(words) < 5 || words[0] != "git" || words[1] != "checkout" || words[3] != "--" || !isHex(words[2], 40) {
		return "", nil, errors.New("before_repo_set_cmd final line is not a pinned verifier checkout")
	}
	if words[2] == baseCommit {
		return "", nil, errors.New("verifier commit must differ from base_commit")
	}
	paths := words[4:]
	seen := make(map[string]struct{}, len(paths))
	for _, file := range paths {
		if !safeRepositoryPath(file) {
			return "", nil, fmt.Errorf("verifier checkout path %q is unsafe", file)
		}
		if _, duplicate := seen[file]; duplicate {
			return "", nil, fmt.Errorf("duplicate verifier checkout path %q", file)
		}
		seen[file] = struct{}{}
	}
	return words[2], paths, nil
}

func parseShellWords(line string) ([]string, error) {
	var words []string
	for index := 0; index < len(line); {
		for index < len(line) && (line[index] == ' ' || line[index] == '\t') {
			index++
		}
		if index == len(line) {
			break
		}
		var word strings.Builder
		started := false
		for index < len(line) && line[index] != ' ' && line[index] != '\t' {
			started = true
			switch line[index] {
			case '\'', '"':
				quote := line[index]
				index++
				for index < len(line) && line[index] != quote {
					if quote == '"' && line[index] == '\\' {
						index++
						if index == len(line) {
							return nil, errors.New("unterminated quoted escape")
						}
					}
					word.WriteByte(line[index])
					index++
				}
				if index == len(line) {
					return nil, errors.New("unterminated quote")
				}
				index++
			case '\\':
				index++
				if index == len(line) {
					return nil, errors.New("unterminated escape")
				}
				word.WriteByte(line[index])
				index++
			default:
				if strings.ContainsRune(";$|&<>`\n\r\x00", rune(line[index])) {
					return nil, errors.New("unsupported shell metacharacter")
				}
				word.WriteByte(line[index])
				index++
			}
		}
		if !started || word.Len() == 0 {
			return nil, errors.New("empty shell word")
		}
		words = append(words, word.String())
	}
	return words, nil
}

func safeRepositoryPath(value string) bool {
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsRune(value, 0) {
		return false
	}
	return true
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
