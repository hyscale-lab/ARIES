package testfixture

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func ReadToolCalls(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("tool-calls line is not JSON: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func DecodeRawAuditRecords(t *testing.T, content []byte) []map[string]string {
	t.Helper()
	const begin = "--- ARIES SSH CALL BEGIN ---\n"
	const end = "--- ARIES SSH CALL END ---\n"
	fields := []string{"sequence", "timestamp", "request_type", "want_reply", "status", "run_id", "task_id", "container_id", "wire_command", "payload_bytes", "payload", "stdin_bytes", "stdin"}
	var records []map[string]string
	for len(content) > 0 {
		if !bytes.HasPrefix(content, []byte(begin)) {
			t.Fatalf("raw audit missing begin delimiter: %q", content)
		}
		content = content[len(begin):]
		record := make(map[string]string, len(fields))
		for _, field := range fields {
			newline := bytes.IndexByte(content, '\n')
			if newline < 0 {
				t.Fatalf("raw audit missing %s line ending: %q", field, content)
			}
			line := string(content[:newline])
			prefix := field + "="
			if !strings.HasPrefix(line, prefix) {
				t.Fatalf("raw audit field order: got %q want prefix %q", line, prefix)
			}
			record[field] = strings.TrimPrefix(line, prefix)
			content = content[newline+1:]
		}
		if !bytes.HasPrefix(content, []byte(end)) {
			t.Fatalf("raw audit missing end delimiter: %q", content)
		}
		content = content[len(end):]
		records = append(records, record)
	}
	return records
}

func UnescapeRawValue(t *testing.T, value string) []byte {
	t.Helper()
	var output []byte
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			_, size := utf8.DecodeRuneInString(value[index:])
			output = append(output, value[index:index+size]...)
			index += size
			continue
		}
		if index+1 >= len(value) {
			t.Fatalf("dangling raw escape in %q", value)
		}
		switch value[index+1] {
		case '\\':
			output = append(output, '\\')
			index += 2
		case 'n':
			output = append(output, '\n')
			index += 2
		case 'r':
			output = append(output, '\r')
			index += 2
		case 't':
			output = append(output, '\t')
			index += 2
		case 'x':
			if index+4 > len(value) {
				t.Fatalf("short raw hex escape in %q", value)
			}
			decoded, err := hex.DecodeString(value[index+2 : index+4])
			if err != nil {
				t.Fatalf("invalid raw hex escape in %q: %v", value, err)
			}
			output = append(output, decoded[0])
			index += 4
		default:
			t.Fatalf("unknown raw escape in %q", value)
		}
	}
	return output
}
