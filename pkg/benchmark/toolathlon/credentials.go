package toolathlon

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
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
// registering the accounts (global_preparation/how2register_accounts.md). The
// adapter keeps the checkout's copy at the example, so the pinned tree stays
// verifiable, and takes the filled file -- with the key files it names -- from
// a directory outside the checkout named by benchmark.toolathlon.credentials_dir.
// That directory is overlaid on the sandbox's configs/ after the project tree
// is installed, at preparation and again after the evaluate-time reinstall,
// so the agent's sandbox and the grader both see the same credentials and
// neither depends on what the agent left behind.

const (
	// credentialsFileName is the one file the directory must hold.
	credentialsFileName = "token_key_session.py"
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
)

// tokenValue is one assignment in a token_key_session.py: the string
// literal when the value is one, or "set by code" when it is an expression
// (the example computes the Google fields from a JSON file, for instance).
type tokenValue struct {
	literal   string
	isLiteral bool
}

// credentials is a credentials directory with its token_key_session.py
// parsed.
type credentials struct {
	dir    string
	values map[string]tokenValue
}

// readCredentials checks the directory and parses its token file.
func readCredentials(dir string) (*credentials, error) {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("toolathlon credentials directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("toolathlon credentials directory %q is not a directory", dir)
	}
	content, err := os.ReadFile(filepath.Join(dir, credentialsFileName))
	if err != nil {
		return nil, fmt.Errorf("toolathlon credentials directory must hold Toolathlon's filled %s: %w", credentialsFileName, err)
	}
	return &credentials{dir: dir, values: parseTokenAssignments(content)}, nil
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

// missing reports which of the keys a server reads the directory does not
// provide: not assigned, still the example's placeholder, or naming a file
// under configs/ that the directory does not hold. A key the task's own
// token_key_session.py assigns (the repositories, pages, or folders that
// task works on) is provided by the task.
func (c *credentials) missing(keys []string, taskOverrides map[string]tokenValue) []string {
	var missing []string
	for _, key := range keys {
		if _, overridden := taskOverrides[key]; overridden {
			continue
		}
		value, ok := c.values[key]
		switch {
		case !ok:
			missing = append(missing, key+" (not set)")
		case !value.isLiteral:
			// Computed by the file's own code: taken as set.
		case value.literal == credentialPlaceholder:
			missing = append(missing, key+" (still the example's placeholder)")
		case strings.HasPrefix(value.literal, credentialsConfigsDir+"/"):
			relative := strings.TrimPrefix(value.literal, credentialsConfigsDir+"/")
			if relative == "" || strings.Contains(relative, "..") || path.IsAbs(relative) {
				missing = append(missing, key+" (not a file under "+credentialsConfigsDir+"/)")
			} else if _, err := os.Stat(filepath.Join(c.dir, filepath.FromSlash(relative))); err != nil {
				missing = append(missing, key+" (file "+value.literal+" is not in the directory)")
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

// writeCredentialsArchive writes the directory as a tar archive whose
// members sit under configs/, so extracting it at workspaceRoot overlays the
// checkout's copies. Bytecode caches are skipped; anything that is not a
// regular file or directory is refused, because nothing Toolathlon's guide
// asks for is one.
func writeCredentialsArchive(dir, destination string) (err error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create credentials archive: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close credentials archive: %w", closeErr)
		}
	}()
	writer := tar.NewWriter(file)
	err = filepath.WalkDir(dir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk credentials directory: %w", walkErr)
		}
		relative, err := filepath.Rel(dir, current)
		if err != nil {
			return fmt.Errorf("relativize %q: %w", current, err)
		}
		if relative == "." {
			return nil
		}
		if entry.Name() == "__pycache__" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := path.Join(credentialsConfigsDir, filepath.ToSlash(relative))
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %q: %w", name, err)
		}
		switch {
		case info.IsDir():
			return writer.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return fmt.Errorf("write header %q: %w", name, err)
			}
			content, err := os.Open(current)
			if err != nil {
				return fmt.Errorf("open %q: %w", name, err)
			}
			defer content.Close()
			if _, err := io.Copy(writer, content); err != nil {
				return fmt.Errorf("archive %q: %w", name, err)
			}
			return nil
		default:
			return fmt.Errorf("credentials directory entry %q is neither a regular file nor a directory", relative)
		}
	})
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish credentials archive: %w", err)
	}
	return nil
}
