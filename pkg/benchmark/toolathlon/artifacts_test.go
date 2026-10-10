package toolathlon

import (
	"slices"
	"strings"
	"testing"
)

func TestParseFindListing(t *testing.T) {
	entries, err := parseFindListing("d\tevaluation\nf\ttask_config.json\nl\tlink\n\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []dirEntry{{'d', "evaluation"}, {'f', "task_config.json"}, {'l', "link"}}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries = %v", entries)
	}
	for _, bad := range []string{"devaluation", "d\t", "d\t.", "d\t..", "dd\tx", "d\ta/b"} {
		if _, err := parseFindListing(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

func TestSelectProtectedEntriesMirrorsTheUpstreamGuard(t *testing.T) {
	selected, err := selectProtectedEntries(fixtureTaskEntries)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(selected))
	for _, entry := range selected {
		names = append(names, entry.name)
	}
	if !slices.Equal(names, []string{"evaluation", "groundtruth_workspace", "preprocess", "README.md"}) {
		t.Fatalf("selected = %v", names)
	}

	withExtras := append(slices.Clone(fixtureTaskEntries), dirEntry{'d', "golden"}, dirEntry{'f', "expected_results.json"}, dirEntry{'f', "Readme.MD"}, dirEntry{'f', "guide.md"})
	selected, err = selectProtectedEntries(withExtras)
	if err != nil {
		t.Fatal(err)
	}
	names = names[:0]
	for _, entry := range selected {
		names = append(names, entry.name)
	}
	want := []string{"evaluation", "expected_results.json", "golden", "groundtruth_workspace", "guide.md", "preprocess", "README.md", "Readme.MD"}
	if !slices.Equal(names, want) {
		t.Fatalf("selected = %v\nwant     %v", names, want)
	}

	if _, err := selectProtectedEntries([]dirEntry{{'d', "docs"}}); err == nil || !strings.Contains(err.Error(), "required protected directory") {
		t.Fatalf("missing evaluation: %v", err)
	}
	if _, err := selectProtectedEntries([]dirEntry{{'f', "evaluation"}}); err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("evaluation as a file: %v", err)
	}
	if _, err := selectProtectedEntries([]dirEntry{{'d', "evaluation"}, {'l', "gt_record.md"}}); err == nil || !strings.Contains(err.Error(), "unexpected type") {
		t.Fatalf("protected file as a symlink: %v", err)
	}
}

func TestRemovalCandidatesCoverPlantedAndStashedNames(t *testing.T) {
	present := []dirEntry{{'d', "evaluation"}, {'f', "readme.MD"}, {'f', "ReadMe.md"}, {'d', "docs"}}
	candidates := removalCandidates(present, []string{"evaluation", "preprocess", "README.md"})
	for _, name := range []string{"evaluation", "preprocess", "golden", "groundtruth_workspace", "gt_record.md", "README.md", "readme.MD", "ReadMe.md"} {
		if !slices.Contains(candidates, name) {
			t.Fatalf("candidates %v lack %q", candidates, name)
		}
	}
	if slices.Contains(candidates, "docs") {
		t.Fatalf("candidates %v include an unprotected entry", candidates)
	}
	if !slices.IsSorted(candidates) {
		t.Fatalf("candidates are not sorted: %v", candidates)
	}
}
