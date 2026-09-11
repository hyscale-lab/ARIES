package toolathlon

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// The protected names below are Toolathlon's own list
// (scripts/containerized/task_artifact_guard.py at the pinned revision): the
// entries directly beneath a task directory that would leak the grader or
// the answer to an agent that can read the task tree. They are stashed after
// preprocess (which needs `preprocess/`) and before the bridge exists, and
// restored only for evaluation. `evaluation` must exist; the rest are taken
// when present.

var protectedDirectories = []string{
	"preprocess",
	"evaluation",
	"groundtruth_workspace",
	"golden",
}

const requiredProtectedDirectory = "evaluation"

var protectedExactFiles = map[string]struct{}{
	"gt_record.md":                     {},
	"expected_results.json":            {},
	"setup_results.json":               {},
	"recalled_products_info.json":      {},
	"test_customers_info.json":         {},
	"create_excel_report.py":           {},
	"send_reminder_emails.py":          {},
	"generate_groundtruth.py":          {},
	"build_excel_ledger.py":            {},
	"verify_groundtruth.py":            {},
	"generate_initial_excel.py":        {},
	"test_evaluation.py":               {},
	"test_evaluation_enhanced.py":      {},
	"test_enhanced_evaluation.py":      {},
	"test_check_local.py":              {},
	"test_integration.py":              {},
	"readme_xiaochen.md":               {},
	"guide.md":                         {},
	"restructure_summary.md":           {},
	"evaluation_enhancement_report.md": {},
	"note.md":                          {},
	"stock_alerts_initial.xlsx":        {},
	"convert_to_backup.py":             {},
	"station_english_name.txt":         {},
}

// protectedCasefoldFiles match any capitalization, because a task README
// often carries the author's solution notes.
var protectedCasefoldFiles = map[string]struct{}{"readme.md": {}}

func isProtectedFileName(name string) bool {
	if _, exact := protectedExactFiles[name]; exact {
		return true
	}
	_, casefold := protectedCasefoldFiles[strings.ToLower(name)]
	return casefold
}

// dirEntry is one direct child of the task directory as reported by find.
type dirEntry struct {
	kind byte // 'd', 'f', 'l', ... as printed by find's %y
	name string
}

// selectProtectedEntries picks the entries to stash from a task directory
// listing, refusing type surprises the way the upstream guard does.
func selectProtectedEntries(entries []dirEntry) ([]dirEntry, error) {
	byName := make(map[string]dirEntry, len(entries))
	for _, entry := range entries {
		byName[entry.name] = entry
	}
	var selected []dirEntry
	for _, name := range protectedDirectories {
		entry, ok := byName[name]
		if !ok {
			if name == requiredProtectedDirectory {
				return nil, fmt.Errorf("required protected directory %q is missing", name)
			}
			continue
		}
		if entry.kind != 'd' {
			return nil, fmt.Errorf("protected directory %q has unexpected type %q", name, string(entry.kind))
		}
		selected = append(selected, entry)
	}
	for _, entry := range entries {
		if !isProtectedFileName(entry.name) {
			continue
		}
		if entry.kind != 'f' {
			return nil, fmt.Errorf("protected file %q has unexpected type %q", entry.name, string(entry.kind))
		}
		selected = append(selected, entry)
	}
	sort.Slice(selected, func(i, j int) bool {
		left, right := strings.ToLower(selected[i].name), strings.ToLower(selected[j].name)
		if left != right {
			return left < right
		}
		return selected[i].name < selected[j].name
	})
	return selected, nil
}

// removalCandidates is every name that must not survive into evaluation:
// the full protected set (so an agent-created `evaluation/` is discarded,
// not merged into), any README spelling present, and everything stashed.
func removalCandidates(present []dirEntry, stashed []string) []string {
	names := make(map[string]struct{})
	for _, name := range protectedDirectories {
		names[name] = struct{}{}
	}
	for name := range protectedExactFiles {
		names[name] = struct{}{}
	}
	for _, entry := range present {
		if _, casefold := protectedCasefoldFiles[strings.ToLower(entry.name)]; casefold {
			names[entry.name] = struct{}{}
		}
	}
	for _, name := range stashed {
		names[name] = struct{}{}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// parseFindListing parses `find -mindepth 1 -maxdepth 1 -printf '%y\t%f\n'`
// output. Names containing a newline cannot be listed this way and are
// rejected rather than mis-split; no pinned task has one.
func parseFindListing(output string) ([]dirEntry, error) {
	var entries []dirEntry
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		kind, name, ok := strings.Cut(line, "\t")
		if !ok || len(kind) != 1 || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\t") {
			return nil, fmt.Errorf("unparseable directory listing line %q", line)
		}
		entries = append(entries, dirEntry{kind: kind[0], name: name})
	}
	return entries, nil
}

func taskDirectoryPath(taskName string) string {
	return path.Join(workspaceRoot, "tasks", taskPool, taskName)
}

// listTaskDirectory returns the direct children of the task directory.
func listTaskDirectory(ctx context.Context, sandbox runner.Sandbox, taskName string) ([]dirEntry, error) {
	result, err := sandbox.Exec(ctx, core.Command{
		Path: "find", Args: []string{taskDirectoryPath(taskName), "-mindepth", "1", "-maxdepth", "1", "-printf", `%y\t%f\n`},
	})
	if err != nil {
		return nil, fmt.Errorf("list task directory: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("list task directory: exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return parseFindListing(result.Stdout)
}

// stashArtifacts archives the protected entries to hostArchive, removes them
// from the task directory, and proves their absence. It returns the stashed
// names for restoreArtifacts.
func stashArtifacts(ctx context.Context, sandbox runner.Sandbox, taskName, hostArchive string) ([]string, error) {
	entries, err := listTaskDirectory(ctx, sandbox, taskName)
	if err != nil {
		return nil, err
	}
	selected, err := selectProtectedEntries(entries)
	if err != nil {
		return nil, fmt.Errorf("select grader artifacts: %w", err)
	}
	names := make([]string, 0, len(selected))
	for _, entry := range selected {
		names = append(names, entry.name)
	}
	taskDir := taskDirectoryPath(taskName)
	archived, err := sandbox.Exec(ctx, core.Command{
		Path: "tar", Args: append([]string{"-C", taskDir, "-cf", stashContainerPath, "--"}, names...),
	})
	if err != nil {
		return nil, fmt.Errorf("archive grader artifacts: %w", err)
	}
	if archived.ExitCode != 0 {
		return nil, fmt.Errorf("archive grader artifacts: exit code %d: %s", archived.ExitCode, strings.TrimSpace(archived.Stderr))
	}
	if err := sandbox.Download(ctx, stashContainerPath, hostArchive); err != nil {
		return nil, fmt.Errorf("download grader artifacts: %w", err)
	}
	members, err := archiveMemberNames(hostArchive)
	if err != nil {
		return nil, fmt.Errorf("inspect downloaded grader artifacts: %w", err)
	}
	for _, name := range names {
		if !slices.ContainsFunc(members, func(member string) bool { return member == name || member == name+"/" }) {
			return nil, fmt.Errorf("downloaded grader archive lacks %q", name)
		}
	}
	paths := make([]string, 0, len(names)+1)
	for _, name := range names {
		paths = append(paths, path.Join(taskDir, name))
	}
	paths = append(paths, stashContainerPath)
	if err := removePaths(ctx, sandbox, paths); err != nil {
		return nil, fmt.Errorf("hide grader artifacts: %w", err)
	}
	return names, nil
}

// restoreArtifacts discards every removal candidate in the task directory,
// re-injects the stashed archive, and confirms each stashed name is back.
func restoreArtifacts(ctx context.Context, sandbox runner.Sandbox, taskName, hostArchive string, stashed []string) error {
	entries, err := listTaskDirectory(ctx, sandbox, taskName)
	if err != nil {
		return err
	}
	taskDir := taskDirectoryPath(taskName)
	candidates := removalCandidates(entries, stashed)
	paths := make([]string, 0, len(candidates)+1)
	for _, name := range candidates {
		paths = append(paths, path.Join(taskDir, name))
	}
	paths = append(paths, stashContainerPath)
	if err := removePaths(ctx, sandbox, paths); err != nil {
		return fmt.Errorf("discard agent-visible grader paths: %w", err)
	}
	if err := sandbox.Upload(ctx, hostArchive, stashContainerPath); err != nil {
		return fmt.Errorf("inject grader artifacts: %w", err)
	}
	extracted, err := sandbox.Exec(ctx, core.Command{Path: "tar", Args: []string{"-C", taskDir, "-xf", stashContainerPath}})
	if err != nil {
		return fmt.Errorf("extract grader artifacts: %w", err)
	}
	if extracted.ExitCode != 0 {
		return fmt.Errorf("extract grader artifacts: exit code %d: %s", extracted.ExitCode, strings.TrimSpace(extracted.Stderr))
	}
	if err := removePaths(ctx, sandbox, []string{stashContainerPath}); err != nil {
		return err
	}
	after, err := listTaskDirectory(ctx, sandbox, taskName)
	if err != nil {
		return err
	}
	for _, name := range stashed {
		if !slices.ContainsFunc(after, func(entry dirEntry) bool { return entry.name == name }) {
			return fmt.Errorf("grader artifact %q did not reappear after restore", name)
		}
	}
	return nil
}

// removePaths deletes paths and separately proves that neither a filesystem
// entry nor a dangling symlink remains, matching the other benchmarks.
func removePaths(ctx context.Context, sandbox runner.Sandbox, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	removed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/rm", Args: append([]string{"-rf", "--"}, paths...)})
	if err != nil {
		return fmt.Errorf("remove paths: %w", err)
	}
	if removed.ExitCode != 0 {
		return fmt.Errorf("remove paths: exit code %d", removed.ExitCode)
	}
	const absencePredicate = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`
	probed, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", absencePredicate, "aries-toolathlon-absence"}, paths...),
	})
	if err != nil {
		return fmt.Errorf("confirm paths absent: %w", err)
	}
	if probed.ExitCode != 0 {
		return errors.New("confirm paths absent: a path still exists")
	}
	return nil
}
