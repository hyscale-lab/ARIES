package sweatlas

import (
	"context"
	"errors"
	"testing"
)

func TestStripNumericPrefix(t *testing.T) {
	tests := map[string]string{
		"1.1: States the port number":       "States the port number",
		"2: A single-level prefix":          "A single-level prefix",
		"No prefix at all":                  "No prefix at all",
		"10.2.3: deeply nested prefix here": "deeply nested prefix here",
	}
	for input, want := range tests {
		if got := stripNumericPrefix(input); got != want {
			t.Errorf("stripNumericPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRenderUserPrompt(t *testing.T) {
	template := "# Prompt\n{problem_statement}\n\n# Response\n{model_answer}\n\n#Rubric Criteria\n{{\n  \"rubric_statement\": {title}\n}}"
	got := renderUserPrompt(template, "the problem", "the answer", `"the title"`)
	want := "# Prompt\nthe problem\n\n# Response\nthe answer\n\n#Rubric Criteria\n{\n  \"rubric_statement\": \"the title\"\n}"
	if got != want {
		t.Fatalf("renderUserPrompt() = %q, want %q", got, want)
	}
}

func TestNormalizeStatus(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{"YES", "YES"}, {"yes", "YES"}, {"Y", "YES"}, {"true", "YES"}, {float64(1), "YES"},
		{"NO", "NO"}, {"no", "NO"}, {"N", "NO"}, {"false", "NO"}, {float64(0), "NO"},
		{"maybe", ""}, {nil, ""},
	}
	for _, test := range tests {
		if got := normalizeStatus(test.value); got != test.want {
			t.Errorf("normalizeStatus(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestNormalizeScore(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{"1", "1"}, {"1.0", "1"}, {float64(1), "1"}, {"yes", "1"}, {"true", "1"},
		{"0", "0"}, {"0.0", "0"}, {float64(0), "0"}, {"no", "0"}, {"false", "0"},
		{"invalid", ""}, {nil, ""},
	}
	for _, test := range tests {
		if got := normalizeScore(test.value); got != test.want {
			t.Errorf("normalizeScore(%#v) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestApplyNegativeFlip(t *testing.T) {
	tests := []struct {
		rawScore, rubricType string
		wantScore            string
		wantFlipped          bool
	}{
		{"1", "", "1", false},
		{"0", "", "0", false},
		{"1", "positive hli verifier", "1", false},
		{"1", "Negative behavior check", "0", true},
		{"0", "negative behavior check", "1", true},
		{"", "negative", "", false},
	}
	for _, test := range tests {
		score, flipped := applyNegativeFlip(test.rawScore, test.rubricType)
		if score != test.wantScore || flipped != test.wantFlipped {
			t.Errorf("applyNegativeFlip(%q, %q) = (%q, %v), want (%q, %v)", test.rawScore, test.rubricType, score, flipped, test.wantScore, test.wantFlipped)
		}
	}
}

func TestCanonicalizeJudgeResultStatusIsCanonicalOverScore(t *testing.T) {
	// status says YES but score says "0" — canonical result should prefer
	// status and record the mismatch, mirroring evaluate_answer.py.
	parsed := rawRating{Status: "YES", Score: "0"}
	result := canonicalizeJudgeResult(parsed, "")
	if result.Score != "1" || result.Status != "YES" {
		t.Fatalf("result = %#v, want status-driven score 1", result)
	}
	if !result.JudgeStatusScoreMismatch {
		t.Fatal("expected judge_status_score_mismatch to be true")
	}
}

func TestCanonicalizeJudgeResultFallsBackToScoreWhenStatusMissing(t *testing.T) {
	parsed := rawRating{Score: "1"}
	result := canonicalizeJudgeResult(parsed, "")
	if result.Score != "1" || result.Status != "YES" || result.JudgeStatusScoreMismatch {
		t.Fatalf("result = %#v, want score-driven YES with no mismatch", result)
	}
}

func TestCanonicalizeJudgeResultAppliesNegativeFlip(t *testing.T) {
	parsed := rawRating{Status: "YES"}
	result := canonicalizeJudgeResult(parsed, "negative behavior")
	if result.Score != "0" || result.Status != "NO" || !result.WasFlipped {
		t.Fatalf("result = %#v, want flipped to NO/0", result)
	}
}

func TestParseJudgeResponse(t *testing.T) {
	valid := `{"ratings":[{"rubric_statement":"s","status":"YES","score":"1","justification":"j"}]}`
	tests := []struct {
		name    string
		content string
		wantOK  bool
	}{
		{"bare json", valid, true},
		{"json fence", "```json\n" + valid + "\n```", true},
		{"prose then json", "Here is my answer:\n" + valid, true},
		{"empty ratings", `{"ratings":[]}`, false},
		{"not json", "this is not json", false},
		{"empty", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rating, ok := parseJudgeResponse(test.content)
			if ok != test.wantOK {
				t.Fatalf("parseJudgeResponse(%q) ok = %v, want %v", test.content, ok, test.wantOK)
			}
			if ok && rating.Status != "YES" {
				t.Fatalf("rating = %#v, want status YES", rating)
			}
		})
	}
}

func TestAggregateRubricResultsMustHaveAllPassAndMeanAggScore(t *testing.T) {
	mustHave := "must have"
	niceToHave := "nice to have"
	rubrics := []rubric{
		{ID: "a", Title: "a", Importance: &mustHave},
		{ID: "b", Title: "b", Importance: &mustHave},
		{ID: "c", Title: "c", Importance: &niceToHave},
	}
	scores := []*canonicalResult{
		{Score: "1"},
		{Score: "1"},
		{Score: "0"},
	}
	results := aggregateRubricResults(rubrics, scores)
	if results.Reward != 1 || !results.Pass {
		t.Fatalf("results = %#v, want reward 1 (all must-haves passed)", results)
	}
	if results.AggScore != 2.0/3.0 {
		t.Fatalf("AggScore = %v, want 2/3 (mean over all 3 scored rubrics)", results.AggScore)
	}
}

func TestAggregateRubricResultsFailsWhenAnyMustHaveFails(t *testing.T) {
	mustHave := "must have"
	rubrics := []rubric{
		{ID: "a", Title: "a", Importance: &mustHave},
		{ID: "b", Title: "b", Importance: &mustHave},
	}
	scores := []*canonicalResult{{Score: "1"}, {Score: "0"}}
	results := aggregateRubricResults(rubrics, scores)
	if results.Reward != 0 || results.Pass {
		t.Fatalf("results = %#v, want reward 0", results)
	}
}

func TestAggregateRubricResultsFailsWhenNoMustHaveScored(t *testing.T) {
	mustHave := "must have"
	rubrics := []rubric{{ID: "a", Title: "a", Importance: &mustHave}}
	scores := []*canonicalResult{nil}
	results := aggregateRubricResults(rubrics, scores)
	if results.Reward != 0 || results.AggScore != 0 {
		t.Fatalf("results = %#v, want reward 0 and agg_score 0 when nothing scored", results)
	}
}

func TestAggregateRubricResultsDefaultsMissingImportanceToMustHave(t *testing.T) {
	rubrics := []rubric{{ID: "a", Title: "a"}}
	scores := []*canonicalResult{{Score: "0"}}
	results := aggregateRubricResults(rubrics, scores)
	if results.RubricScores[0].Importance != "must have" {
		t.Fatalf("importance = %q, want default must have", results.RubricScores[0].Importance)
	}
	if results.Reward != 0 {
		t.Fatalf("Reward = %v, want 0 (the only must-have rubric scored 0)", results.Reward)
	}
}

func TestEvaluateSingleRubricRetriesOnErrorThenSucceeds(t *testing.T) {
	restoreDelay, restoreCap := rubricRetryBaseDelay, rubricRetryCap
	rubricRetryBaseDelay, rubricRetryCap = 0, 0
	defer func() { rubricRetryBaseDelay, rubricRetryCap = restoreDelay, restoreCap }()

	chat := &stubChat{
		errs:      []error{errors.New("transient")},
		responses: []string{"", ratingResponse("YES")},
	}
	result, err := evaluateSingleRubric(context.Background(), chat, "system", "{title}", "problem", "answer", rubric{ID: "r", Title: "r"})
	if err != nil || !result.isScored() || result.Score != "1" {
		t.Fatalf("result = %#v, err = %v, want scored YES after one retry", result, err)
	}
	if chat.calls != 2 {
		t.Fatalf("chat.calls = %d, want 2", chat.calls)
	}
}

func TestEvaluateSingleRubricExhaustsRetriesOnPersistentInvalidResponse(t *testing.T) {
	responses := make([]string, maxRubricRetries)
	for i := range responses {
		responses[i] = "not json"
	}
	chat := &stubChat{responses: responses}
	result, err := evaluateSingleRubric(context.Background(), chat, "system", "{title}", "problem", "answer", rubric{ID: "r", Title: "r"})
	if result != nil {
		t.Fatalf("result = %#v, want nil after exhausting retries", result)
	}
	if err == nil {
		t.Fatal("expected a diagnostic error after exhausting retries")
	}
	if chat.calls != maxRubricRetries {
		t.Fatalf("chat.calls = %d, want %d", chat.calls, maxRubricRetries)
	}
}
