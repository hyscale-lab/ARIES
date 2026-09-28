package toolathlon

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Account-backed servers (GitHub, Google, Hugging Face, Notion, Snowflake,
// W&B, YouTube) read their tokens from Toolathlon's configs/token_key_session.py,
// a gitignored file the user derives from token_key_session_example.py after
// registering the accounts (global_preparation/how2register_accounts.md), and
// some of them read key files under configs/ that it names. The adapter keeps
// the checkout's copies at the example, so the pinned tree stays verifiable,
// and takes the values from the host environment, as ARIES takes a model's
// API key: benchmark.toolathlon.credentials_env maps each token field, and
// credential_files_env each key file, to the name of an environment variable,
// so a profile and every saved configuration hold names only. At task load
// the adapter writes the token file from the checkout's example with those
// fields filled in. It is overlaid on the sandbox's configs/ together with
// the key files after the project tree is installed, at preparation and again
// after the evaluate-time reinstall, so the agent's sandbox and the grader
// both see the same credentials and neither depends on what the agent left
// behind. Every value taken from the environment is scrubbed from what the
// adapter saves (scrub.go).

const (
	// credentialsFileName is the token file Toolathlon's servers read.
	credentialsFileName = "token_key_session.py"
	// credentialsExampleName is the checkout's example that the token file
	// is written from.
	credentialsExampleName = "token_key_session_example.py"
	// credentialsArchiveContainerPath is where the overlay archive lands
	// before extraction over workspaceRoot.
	credentialsArchiveContainerPath = privateRoot + "/credentials.tar"
	// credentialPlaceholder is the value the example leaves in every
	// account field ("TO BE FILLED").
	credentialPlaceholder = "XX"
	// credentialsConfigsDir is where the files live in the checkout and in
	// the sandbox, relative to the project root.
	credentialsConfigsDir = "configs"
)

// serverBinaries are checkout entries a server needs in the sandbox beyond
// the project code, which the project archive omits for every other task:
// the GitHub server is a Go binary committed under local_binary.
var serverBinaries = map[string][]string{
	"github": {"local_binary/github-mcp-server"},
}

var (
	// tokenAssignment matches one `key = value` line of a
	// token_key_session.py; the rest of the line is the value expression.
	tokenAssignment = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)
	// stringLiteral matches a value that starts with a Python string
	// literal in either quote.
	stringLiteral = regexp.MustCompile(`^(?:"([^"]*)"|'([^']*)')`)
	// tokenReference matches the `${token.<key>}` substitutions in a server
	// file: the fields Toolathlon reads for that server.
	tokenReference = regexp.MustCompile(`\$\{token\.([A-Za-z0-9_]+)\}`)
	// identifier is a token field name, and also the shape required of an
	// environment variable named in the profile.
	identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// tokenValue is one assignment in a token_key_session.py: the string
// literal when the value is one, or "set by code" when it is an expression
// (the example computes the Google fields from a JSON file, for instance).
type tokenValue struct {
	literal   string
	isLiteral bool
}

// credentials are the token file written for the sandbox, the key files,
// and what the environment did not provide.
type credentials struct {
	// tokenFile is the checkout's example with the configured fields filled.
	tokenFile []byte
	values    map[string]tokenValue
	// files maps a key file's path relative to configs/ to its content.
	files map[string][]byte
	// unset maps a field, or a key file's configs/ path, to the environment
	// variable the profile names for it and the environment does not set.
	unset map[string]string
	// secrets are the values taken from the environment, for scrubbing.
	secrets []string
}

// validateCredentialNames checks the profile's two maps before any value is
// read: token fields are Python identifiers, key files are paths under
// configs/ other than the token file, and every value names an environment
// variable.
func validateCredentialNames(fields, files map[string]string) error {
	for field, variable := range fields {
		if !identifier.MatchString(field) {
			return fmt.Errorf("toolathlon credentials_env key %q is not a token field name", field)
		}
		if !identifier.MatchString(variable) {
			return fmt.Errorf("toolathlon credentials_env[%q] must name an environment variable, not %q", field, variable)
		}
	}
	for file, variable := range files {
		if _, ok := configsRelative(file); !ok {
			return fmt.Errorf("toolathlon credential_files_env key %q must be a file path under %s/ other than %s", file, credentialsConfigsDir, credentialsFileName)
		}
		if !identifier.MatchString(variable) {
			return fmt.Errorf("toolathlon credential_files_env[%q] must name an environment variable, not %q", file, variable)
		}
	}
	return nil
}

// configsRelative returns a configs/ path relative to configs/, refusing
// paths that leave it and the token file itself.
func configsRelative(file string) (string, bool) {
	relative, found := strings.CutPrefix(file, credentialsConfigsDir+"/")
	if !found || relative == "" || path.IsAbs(relative) || path.Clean(relative) != relative ||
		relative == ".." || strings.HasPrefix(relative, "../") || relative == credentialsFileName {
		return "", false
	}
	return relative, true
}

// resolveCredentials reads the configured values through lookup and writes
// the token file from the checkout's example. A variable that is not set,
// or set to nothing, leaves its field or file unprovided; task load then
// refuses only the tasks that need it.
func resolveCredentials(root string, fields, files map[string]string, lookup func(string) ([]byte, bool)) (*credentials, error) {
	example, err := os.ReadFile(filepath.Join(root, credentialsConfigsDir, credentialsExampleName))
	if err != nil {
		return nil, fmt.Errorf("read Toolathlon's %s: %w", credentialsExampleName, err)
	}
	creds := &credentials{files: make(map[string][]byte), unset: make(map[string]string)}
	filled := make(map[string]string, len(fields))
	for _, field := range sortedKeys(fields) {
		value, ok := lookupValue(lookup, fields[field])
		if !ok {
			creds.unset[field] = fields[field]
			continue
		}
		filled[field] = value
		creds.secrets = append(creds.secrets, value)
	}
	if creds.tokenFile, err = fillTokenFile(example, filled); err != nil {
		return nil, err
	}
	creds.values = parseTokenAssignments(creds.tokenFile)
	for _, file := range sortedKeys(files) {
		relative, _ := configsRelative(file)
		value, ok := lookupValue(lookup, files[file])
		if !ok {
			creds.unset[file] = files[file]
			continue
		}
		creds.files[relative] = []byte(value)
		creds.secrets = append(creds.secrets, value)
	}
	return creds, nil
}

// lookupValue reads one environment variable through the profile's lookup.
// The lookup hands over its buffer, which is cleared once copied.
func lookupValue(lookup func(string) ([]byte, bool), name string) (string, bool) {
	if lookup == nil {
		return "", false
	}
	raw, ok := lookup(name)
	if !ok || len(raw) == 0 {
		return "", false
	}
	value := string(raw)
	clear(raw)
	return value, true
}

// fillTokenFile writes each filled field's value over the string literals
// the example assigns to it, encoded as a JSON string, which is also a valid
// Python string literal. A field the example does not assign a string to is
// refused: its value is computed by the file's own code (the Google client
// fields read google_credentials.json, which credential_files_env provides).
func fillTokenFile(example []byte, filled map[string]string) ([]byte, error) {
	lines := strings.Split(string(example), "\n")
	replaced := make(map[string]bool, len(filled))
	for index, line := range lines {
		match := tokenAssignment.FindStringSubmatchIndex(line)
		if match == nil {
			continue
		}
		field := line[match[2]:match[3]]
		value, wanted := filled[field]
		if !wanted {
			continue
		}
		rest := line[match[4]:match[5]]
		literal := stringLiteral.FindStringIndex(rest)
		if literal == nil {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode token field %q: %w", field, err)
		}
		lines[index] = line[:match[4]] + string(encoded) + rest[literal[1]:]
		replaced[field] = true
	}
	for _, field := range sortedKeys(filled) {
		if !replaced[field] {
			return nil, fmt.Errorf("token field %q is not assigned a string in Toolathlon's %s (a computed field takes the file it reads from credential_files_env)", field, credentialsExampleName)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// parseTokenAssignments scans a token_key_session.py line by line for
// `key = value` assignments. The file is a Python module whose values are
// mostly string literals, so a line scan is enough to tell a filled field
// from the example's placeholder; a value that is an expression counts as
// set.
func parseTokenAssignments(content []byte) map[string]tokenValue {
	values := make(map[string]tokenValue)
	for _, line := range strings.Split(string(content), "\n") {
		match := tokenAssignment.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, rest := match[1], match[2]
		if literal := stringLiteral.FindStringSubmatch(rest); literal != nil {
			values[key] = tokenValue{literal: literal[1] + literal[2], isLiteral: true}
			continue
		}
		values[key] = tokenValue{}
	}
	return values
}

// missing reports which of the keys a server reads the environment does not
// provide: a variable the profile names but the environment does not set, a
// field the profile does not map (still the example's placeholder), or a
// key file under configs/ the profile does not map. A key the task's own
// token_key_session.py assigns (the repositories, pages, or folders that
// task works on) is provided by the task.
func (c *credentials) missing(keys []string, taskOverrides map[string]tokenValue) []string {
	var missing []string
	for _, key := range keys {
		if _, overridden := taskOverrides[key]; overridden {
			continue
		}
		if variable, unset := c.unset[key]; unset {
			missing = append(missing, key+" (environment variable "+variable+" is not set)")
			continue
		}
		value, ok := c.values[key]
		switch {
		case !ok:
			missing = append(missing, key+" (not in the token file)")
		case !value.isLiteral:
			// Computed by the file's own code: taken as set.
		case value.literal == credentialPlaceholder:
			missing = append(missing, key+" (not in benchmark.toolathlon.credentials_env)")
		case strings.HasPrefix(value.literal, credentialsConfigsDir+"/"):
			relative, ok := configsRelative(value.literal)
			if !ok {
				missing = append(missing, key+" (not a file under "+credentialsConfigsDir+"/)")
			} else if c.files[relative] == nil {
				if variable, unset := c.unset[value.literal]; unset {
					missing = append(missing, key+" (file "+value.literal+": environment variable "+variable+" is not set)")
				} else {
					missing = append(missing, key+" (file "+value.literal+" is not in benchmark.toolathlon.credential_files_env)")
				}
			}
		}
	}
	return missing
}

// serverTokenKeys lists the token fields one server file substitutes,
// sorted and without repeats.
func serverTokenKeys(serverFile string) ([]string, error) {
	content, err := os.ReadFile(serverFile)
	if err != nil {
		return nil, fmt.Errorf("read MCP server file: %w", err)
	}
	seen := make(map[string]struct{})
	for _, match := range tokenReference.FindAllStringSubmatch(string(content), -1) {
		seen[match[1]] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// taskTokenOverrides parses the task's own token_key_session.py, when it
// has one. Toolathlon loads it over the global file for that task.
func taskTokenOverrides(taskDir string) (map[string]tokenValue, error) {
	content, err := os.ReadFile(filepath.Join(taskDir, credentialsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the task's %s: %w", credentialsFileName, err)
	}
	return parseTokenAssignments(content), nil
}

// writeCredentialsArchive writes the token file and the key files as a tar
// archive whose members sit under configs/, so extracting it at
// workspaceRoot overlays the checkout's copies. The archive is written only
// for the upload and removed with it (extractArchive).
func writeCredentialsArchive(creds *credentials, destination string) (err error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create credentials archive: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close credentials archive: %w", closeErr)
		}
	}()
	members := map[string][]byte{credentialsFileName: creds.tokenFile}
	directories := make(map[string]struct{})
	for relative, content := range creds.files {
		members[relative] = content
		for dir := path.Dir(relative); dir != "."; dir = path.Dir(dir) {
			directories[dir] = struct{}{}
		}
	}
	writer := tar.NewWriter(file)
	for _, dir := range sortedKeys(directories) {
		if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: path.Join(credentialsConfigsDir, dir) + "/", Mode: 0o755}); err != nil {
			return fmt.Errorf("write header %q: %w", dir, err)
		}
	}
	for _, relative := range sortedKeys(members) {
		name := path.Join(credentialsConfigsDir, relative)
		content := members[relative]
		if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return fmt.Errorf("write header %q: %w", name, err)
		}
		if _, err := writer.Write(content); err != nil {
			return fmt.Errorf("archive %q: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish credentials archive: %w", err)
	}
	return nil
}

// sortedKeys returns a map's keys in order, so archives and errors do not
// depend on map iteration.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
