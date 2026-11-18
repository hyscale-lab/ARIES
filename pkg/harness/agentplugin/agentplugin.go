// Package agentplugin reads portable plugin packages in the Agent Plugins
// 1.0.0 format (https://agent-plugins.org): a plugin.json manifest, Agent
// Skills under skills/, and MCP servers in mcp.json. It prepares a package for
// a harness runtime as a harness.Plugin.
//
// The specification lets a client skip an invalid skill or server and load the
// rest. ARIES rejects the whole package instead: a run that silently lacks
// part of a selected plugin would not be the experiment the profile describes.
package agentplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/harness"
)

const (
	pluginSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	mcpSchema    = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
	// MaxBytes bounds one package's staged files. Every task occurrence
	// uploads them, so they count in harness start time; a larger plugin
	// belongs in a derived image.
	MaxBytes = 32 << 20
	// MaxFiles bounds one package's staged file count.
	MaxFiles       = 4096
	maxDescription = 1024
)

// manifestFields are the closed set of plugin.json top-level fields (§5.2).
var manifestFields = map[string]bool{
	"$schema": true, "name": true, "version": true, "description": true, "author": true,
	"homepage": true, "repository": true, "license": true, "keywords": true, "extensions": true,
}

// Load reads and validates the package at root, whose manifest name must be
// name. source records the pin it came from.
func Load(root, name, source string) (harness.Plugin, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return harness.Plugin{}, fmt.Errorf("plugin %q: resolve package root: %w", name, err)
	}
	files, err := readTree(resolved)
	if err != nil {
		return harness.Plugin{}, fmt.Errorf("plugin %q: %w", name, err)
	}
	byPath := make(map[string]harness.PluginFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	manifest, ok := byPath["plugin.json"]
	if !ok {
		return harness.Plugin{}, fmt.Errorf("plugin %q: plugin.json is missing", name)
	}
	version, err := validateManifest(manifest.Content, name)
	if err != nil {
		return harness.Plugin{}, fmt.Errorf("plugin %q: plugin.json: %w", name, err)
	}
	skills, err := discoverSkills(files, byPath)
	if err != nil {
		return harness.Plugin{}, fmt.Errorf("plugin %q: %w", name, err)
	}
	var servers []core.MCPServerConfig
	if config, ok := byPath["mcp.json"]; ok {
		servers, err = mcpServers(config.Content, name, byPath)
		if err != nil {
			return harness.Plugin{}, fmt.Errorf("plugin %q: mcp.json: %w", name, err)
		}
	}
	return harness.Plugin{
		Name: name, Version: version, Source: source, TreeSHA256: treeHash(files),
		Files: files, Skills: skills, MCPServers: servers,
	}, nil
}

// readTree returns the package's regular files in path order. A version
// control directory at the root is not part of the package. Symbolic links
// are refused, so every staged path is a plain file inside the root.
func readTree(root string) ([]harness.PluginFile, error) {
	var files []harness.PluginFile
	total := 0
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("package path %q is a symbolic link; ARIES stages regular files only", relative)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("package path %q is not a regular file", relative)
		}
		if strings.ContainsFunc(relative, unicode.IsControl) {
			return fmt.Errorf("package path %q contains a control character", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += int(info.Size())
		if total > MaxBytes || len(files) >= MaxFiles {
			return fmt.Errorf("package exceeds %d files or %d MiB; build it into the harness image instead", MaxFiles, MaxBytes>>20)
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		files = append(files, harness.PluginFile{Path: relative, Content: content, Executable: info.Mode().Perm()&0o111 != 0})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read package: %w", err)
	}
	return files, nil
}

// treeHash identifies the staged content: every path, its executable bit, and
// its bytes, in path order.
func treeHash(files []harness.PluginFile) string {
	sum := sha256.New()
	for _, file := range files {
		content := sha256.Sum256(file.Content)
		fmt.Fprintf(sum, "%s\x00%t\x00%x\n", file.Path, file.Executable, content)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// validateManifest checks plugin.json against §5 and returns its version.
// Unknown top-level fields are ignored, as the specification requires.
func validateManifest(content []byte, name string) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return "", fmt.Errorf("not a JSON object: %w", err)
	}
	var schema, manifestName string
	if err := json.Unmarshal(fields["$schema"], &schema); err != nil || schema != pluginSchema {
		return "", fmt.Errorf("$schema must be %s", pluginSchema)
	}
	if err := json.Unmarshal(fields["name"], &manifestName); err != nil || !core.ValidPluginName(manifestName) {
		return "", errors.New("name must be 1-64 lowercase letters, digits, hyphens or periods, starting and ending alphanumeric")
	}
	if manifestName != name {
		return "", fmt.Errorf("name %q does not match the catalog name %q", manifestName, name)
	}
	var version string
	for field, raw := range fields {
		if !manifestFields[field] {
			continue
		}
		var err error
		switch field {
		case "version":
			err = json.Unmarshal(raw, &version)
		case "description", "homepage", "repository", "license":
			var value string
			err = json.Unmarshal(raw, &value)
		case "keywords":
			var value []string
			err = json.Unmarshal(raw, &value)
		case "author":
			var author map[string]json.RawMessage
			err = json.Unmarshal(raw, &author)
			for key, value := range author {
				var text string
				if (key != "name" && key != "email" && key != "url") || json.Unmarshal(value, &text) != nil {
					err = fmt.Errorf("author field %q is not allowed or not a string", key)
				}
			}
		}
		if err != nil {
			return "", fmt.Errorf("field %q: %w", field, err)
		}
	}
	return version, nil
}

// discoverSkills returns the names of skills/<name>/SKILL.md, each checked
// against the Agent Skills frontmatter rules.
func discoverSkills(files []harness.PluginFile, byPath map[string]harness.PluginFile) ([]string, error) {
	var skills []string
	seen := make(map[string]bool)
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "skills/") {
			continue
		}
		directory := strings.SplitN(strings.TrimPrefix(file.Path, "skills/"), "/", 2)[0]
		if seen[directory] || !strings.Contains(strings.TrimPrefix(file.Path, "skills/"), "/") {
			continue
		}
		seen[directory] = true
		skill, ok := byPath["skills/"+directory+"/SKILL.md"]
		if !ok {
			continue
		}
		if err := validateSkill(skill.Content, directory); err != nil {
			return nil, fmt.Errorf("skill %q: %w", directory, err)
		}
		skills = append(skills, directory)
	}
	slices.Sort(skills)
	return skills, nil
}

func validateSkill(content []byte, directory string) error {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return errors.New("SKILL.md has no YAML frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return errors.New("SKILL.md frontmatter is not closed")
	}
	var frontmatter struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &frontmatter); err != nil {
		return fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if frontmatter.Name != directory || !validSkillName(frontmatter.Name) {
		return fmt.Errorf("SKILL.md name %q must match its directory and use lowercase letters, digits and single hyphens", frontmatter.Name)
	}
	if strings.TrimSpace(frontmatter.Description) == "" || utf8.RuneCountInString(frontmatter.Description) > maxDescription {
		return fmt.Errorf("SKILL.md description must be 1-%d characters", maxDescription)
	}
	return nil
}

func validSkillName(name string) bool {
	if name == "" || len(name) > 64 || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') })
}

// mcpServer is one mcp.json server entry; the variant fields are checked
// against its type.
type mcpServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

var serverFields = map[string]map[string]bool{
	"stdio":           {"type": true, "command": true, "args": true, "env": true, "cwd": true},
	"streamable-http": {"type": true, "url": true, "headers": true},
	"sse":             {"type": true, "url": true, "headers": true},
}

// mcpServers translates mcp.json (§7.2) into runtime MCP server
// configurations for the plugin's staged root and data directory.
func mcpServers(content []byte, plugin string, byPath map[string]harness.PluginFile) ([]core.MCPServerConfig, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	for field := range document {
		if field != "$schema" && field != "mcpServers" {
			return nil, fmt.Errorf("top-level field %q is not allowed", field)
		}
	}
	var schema string
	if err := json.Unmarshal(document["$schema"], &schema); err != nil || schema != mcpSchema {
		return nil, fmt.Errorf("$schema must be %s", mcpSchema)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(document["mcpServers"], &entries); err != nil || entries == nil {
		return nil, errors.New("mcpServers must be an object")
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	slices.Sort(names)
	servers := make([]core.MCPServerConfig, 0, len(names))
	for _, name := range names {
		server, err := mcpServerConfig(name, entries[name], plugin, byPath)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", name, err)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func mcpServerConfig(name string, raw json.RawMessage, plugin string, byPath map[string]harness.PluginFile) (core.MCPServerConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return core.MCPServerConfig{}, errors.New("not a JSON object")
	}
	var entry mcpServer
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&entry); err != nil {
		return core.MCPServerConfig{}, err
	}
	allowed, ok := serverFields[entry.Type]
	if !ok {
		return core.MCPServerConfig{}, fmt.Errorf("type %q is not stdio, streamable-http or sse", entry.Type)
	}
	for field := range fields {
		if !allowed[field] {
			return core.MCPServerConfig{}, fmt.Errorf("field %q does not belong to a %s server", field, entry.Type)
		}
	}
	root, data := harness.PluginRoot(plugin), harness.PluginDataDir(plugin)
	server := core.MCPServerConfig{Name: name}
	if entry.Type == "stdio" {
		command, err := stdioCommand(entry.Command, root, byPath)
		if err != nil {
			return core.MCPServerConfig{}, err
		}
		server.Command = command
		for _, argument := range entry.Args {
			server.Args = append(server.Args, expand(argument, root, data))
		}
		for key, value := range entry.Env {
			if server.Env == nil {
				server.Env = make(map[string]string, len(entry.Env))
			}
			if key == "PLUGIN_ROOT" || key == "PLUGIN_DATA" {
				return core.MCPServerConfig{}, fmt.Errorf("env must not set %s", key)
			}
			server.Env[key] = expand(value, root, data)
		}
		// §9.1: the client supplies these to every stdio server.
		if server.Env == nil {
			server.Env = make(map[string]string, 2)
		}
		server.Env["PLUGIN_ROOT"], server.Env["PLUGIN_DATA"] = root, data
		cwd, err := workingDirectory(entry.Cwd, root, data)
		if err != nil {
			return core.MCPServerConfig{}, err
		}
		server.Cwd = cwd
	} else {
		if err := validateRemoteURL(entry.URL); err != nil {
			return core.MCPServerConfig{}, err
		}
		server.URL, server.Transport, server.Headers = entry.URL, entry.Type, entry.Headers
	}
	// Both harnesses expand ${NAME} in their configuration from the runtime's
	// environment, which holds ARIES's credentials. Only the two plugin
	// placeholders are defined, and they are expanded above.
	if field := unexpandedReference(server); field != "" {
		return core.MCPServerConfig{}, fmt.Errorf("%s contains a ${...} reference; only ${PLUGIN_ROOT} and ${PLUGIN_DATA} are expanded", field)
	}
	if err := core.ValidateMCPServer(server); err != nil {
		return core.MCPServerConfig{}, err
	}
	return server, nil
}

// stdioCommand accepts a bare executable name, resolved on the runtime's
// PATH, or a ./ path to an executable file in the package.
func stdioCommand(command, root string, byPath map[string]harness.PluginFile) (string, error) {
	if command == "" || strings.ContainsFunc(command, unicode.IsSpace) {
		return "", errors.New("command must be one executable token")
	}
	if !strings.HasPrefix(command, "./") {
		if strings.Contains(command, "/") {
			return "", errors.New("command must be a bare name or a ./ path in the package")
		}
		return command, nil
	}
	relative := path.Clean(strings.TrimPrefix(command, "./"))
	if relative == "." || strings.HasPrefix(relative, "../") || relative == ".." {
		return "", errors.New("command leaves the package")
	}
	file, ok := byPath[relative]
	if !ok || !file.Executable {
		return "", fmt.Errorf("command %q is not an executable file in the package", command)
	}
	return root + "/" + relative, nil
}

// workingDirectory resolves cwd (§7.2.1); the default is the plugin root.
func workingDirectory(cwd, root, data string) (string, error) {
	switch {
	case cwd == "":
		return root, nil
	case strings.HasPrefix(cwd, "./"):
		return contained(root, root+"/"+strings.TrimPrefix(cwd, "./"))
	case cwd == "${PLUGIN_ROOT}" || strings.HasPrefix(cwd, "${PLUGIN_ROOT}/"):
		return contained(root, root+strings.TrimPrefix(cwd, "${PLUGIN_ROOT}"))
	case cwd == "${PLUGIN_DATA}" || strings.HasPrefix(cwd, "${PLUGIN_DATA}/"):
		return contained(data, data+strings.TrimPrefix(cwd, "${PLUGIN_DATA}"))
	default:
		return "", errors.New("cwd must start with ./, ${PLUGIN_ROOT} or ${PLUGIN_DATA}")
	}
}

func contained(base, candidate string) (string, error) {
	cleaned := path.Clean(candidate)
	if cleaned != base && !strings.HasPrefix(cleaned, base+"/") {
		return "", errors.New("cwd leaves its base directory")
	}
	return cleaned, nil
}

// expand performs the single, non-recursive placeholder replacement of §9.2.
func expand(value, root, data string) string {
	return strings.NewReplacer("${PLUGIN_ROOT}", root, "${PLUGIN_DATA}", data).Replace(value)
}

func unexpandedReference(server core.MCPServerConfig) string {
	if strings.Contains(server.Command, "${") {
		return "command"
	}
	if strings.Contains(server.Cwd, "${") {
		return "cwd"
	}
	for _, argument := range server.Args {
		if strings.Contains(argument, "${") {
			return "args"
		}
	}
	for key, value := range server.Env {
		if strings.Contains(value, "${") {
			return "env " + key
		}
	}
	if strings.Contains(server.URL, "${") {
		return "url"
	}
	for key, value := range server.Headers {
		if strings.Contains(key+value, "${") {
			return "header " + key
		}
	}
	return ""
}

// validateRemoteURL applies §7.2.1: absolute HTTP(S), no user information or
// fragment, and HTTPS unless the host is loopback.
func validateRemoteURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("url must be an absolute HTTP(S) URL without user information or a fragment")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("url must use HTTPS unless its host is loopback")
	default:
		return errors.New("url must be HTTP(S)")
	}
}
