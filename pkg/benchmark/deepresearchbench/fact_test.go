package deepresearchbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeChatter dispatches canned responses based on which FACT stage prompt
// it recognizes (extract/dedupe/validate each have a distinctive phrase),
// so a single fake can drive a full pipeline.Run() without an HTTP server.
type fakeChatter struct {
	extractResponse  string
	dedupResponse    string
	validateResponse string
	// validateResponses, if set, is consumed in order (one entry per
	// validate() call) instead of always returning validateResponse.
	validateResponses []string
	validateCalls     int
	err               error
}

func (f *fakeChatter) Chat(_ context.Context, _, userPrompt string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	switch {
	case strings.Contains(userPrompt, "Please identify **all** instances"):
		return f.extractResponse, nil
	case strings.Contains(userPrompt, "de-duplicate"):
		return f.dedupResponse, nil
	case strings.Contains(userPrompt, "Begin the assessment now"):
		if len(f.validateResponses) > 0 {
			response := f.validateResponses[f.validateCalls%len(f.validateResponses)]
			f.validateCalls++
			return response, nil
		}
		f.validateCalls++
		return f.validateResponse, nil
	default:
		return "", errors.New("fakeChatter: unrecognized prompt")
	}
}

// fakeJina is an in-memory jinaScraper fake keyed by URL.
type fakeJina struct {
	content map[string]string
	errs    map[string]error
}

func (f *fakeJina) Fetch(_ context.Context, url string) (string, error) {
	if err, ok := f.errs[url]; ok {
		return "", err
	}
	return f.content[url], nil
}

func TestFactPipelineHappyPath(t *testing.T) {
	chat := &fakeChatter{
		extractResponse: `[{"fact":"claim A","url":"https://a.example"},{"fact":"claim B","url":"https://b.example"}]`,
		validateResponses: []string{
			`[{"idx":1,"result":"supported"}]`,
			`[{"idx":1,"result":"unsupported"}]`,
		},
	}
	jina := &fakeJina{content: map[string]string{
		"https://a.example": "content supporting claim A",
		"https://b.example": "unrelated content",
	}}
	pipeline := &factPipeline{chat: chat, jina: jina}

	report, err := pipeline.Run(context.Background(), "an article citing [1] and [2]")
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasCitations {
		t.Fatal("HasCitations = false, want true")
	}
	if report.TotalCitations != 2 {
		t.Fatalf("TotalCitations = %d, want 2", report.TotalCitations)
	}
	if report.EffectiveCitations != 1 {
		t.Fatalf("EffectiveCitations = %v, want 1", report.EffectiveCitations)
	}
}

func TestFactPipelineZeroCitationsShortCircuits(t *testing.T) {
	chat := &fakeChatter{extractResponse: `[]`}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}

	report, err := pipeline.Run(context.Background(), "an article with no citations")
	if err != nil {
		t.Fatal(err)
	}
	if report.HasCitations {
		t.Fatal("HasCitations = true, want false for an article with no extracted citations")
	}
	if report.TotalCitations != 0 || report.EffectiveCitations != 0 {
		t.Fatalf("report = %+v, want zero counts", report)
	}
}

func TestFactPipelineScrapeFailureStillValidates(t *testing.T) {
	chat := &fakeChatter{
		extractResponse:  `[{"fact":"claim A","url":"https://a.example"}]`,
		validateResponse: `[{"idx":1,"result":"unsupported"}]`,
	}
	jina := &fakeJina{errs: map[string]error{"https://a.example": errors.New("connection refused")}}
	pipeline := &factPipeline{chat: chat, jina: jina}

	report, err := pipeline.Run(context.Background(), "an article citing [1]")
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalCitations != 1 || report.EffectiveCitations != 0 {
		t.Fatalf("report = %+v, want TotalCitations=1 EffectiveCitations=0", report)
	}
}

func TestFactPipelineValidateNilContentIsAllUnknown(t *testing.T) {
	pipeline := &factPipeline{chat: &fakeChatter{}, jina: &fakeJina{}}
	groups := map[string]*factURLGroup{
		"https://a.example": {Facts: []string{"claim A", "claim B"}, URLContent: nil},
	}
	results, warnings, err := pipeline.validate(context.Background(), groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for the nil-content short-circuit", warnings)
	}
	classifications := results["https://a.example"]
	if len(classifications) != 2 || classifications[0] != factUnknown || classifications[1] != factUnknown {
		t.Fatalf("classifications = %v, want all unknown", classifications)
	}
}

func TestFactPipelineDeduplicateSkipsLLMForSingleFactURL(t *testing.T) {
	chat := &fakeChatter{err: errors.New("dedup should not be called for a single-fact URL")}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	groups, warnings := pipeline.deduplicate(context.Background(), []factCitation{{Fact: "only claim", URL: "https://a.example"}})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for the skipped-LLM single-fact case", warnings)
	}
	if len(groups["https://a.example"].Facts) != 1 {
		t.Fatalf("facts = %v, want the single original fact untouched", groups["https://a.example"].Facts)
	}
}

func TestFactPipelineDeduplicateCallsLLMForMultiFactURL(t *testing.T) {
	chat := &fakeChatter{dedupResponse: `[1]`}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	groups, warnings := pipeline.deduplicate(context.Background(), []factCitation{
		{Fact: "claim A", URL: "https://a.example"},
		{Fact: "claim A restated", URL: "https://a.example"},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a clean dedup", warnings)
	}
	if got := groups["https://a.example"].Facts; len(got) != 1 || got[0] != "claim A" {
		t.Fatalf("facts = %v, want [\"claim A\"] after dedup", got)
	}
}

func TestFactPipelineDeduplicateKeepsAllFactsAndWarnsOnFailedCall(t *testing.T) {
	chat := &fakeChatter{err: errors.New("dedup unavailable")}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	citations := []factCitation{
		{Fact: "claim A", URL: "https://a.example"},
		{Fact: "claim B", URL: "https://a.example"},
	}
	groups, warnings := pipeline.deduplicate(context.Background(), citations)
	if got := groups["https://a.example"].Facts; len(got) != 2 {
		t.Fatalf("facts = %v, want both facts kept when the dedup call fails", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "https://a.example") {
		t.Fatalf("warnings = %v, want one warning naming the URL", warnings)
	}
}

func TestFactPipelineDeduplicateKeepsAllFactsAndWarnsOnUnparseableResponse(t *testing.T) {
	chat := &fakeChatter{dedupResponse: "not json"}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	citations := []factCitation{
		{Fact: "claim A", URL: "https://a.example"},
		{Fact: "claim B", URL: "https://a.example"},
	}
	groups, warnings := pipeline.deduplicate(context.Background(), citations)
	if got := groups["https://a.example"].Facts; len(got) != 2 {
		t.Fatalf("facts = %v, want both facts kept when the dedup response is unparseable", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning", warnings)
	}
}

func TestFactPipelineDeduplicateKeepsAllFactsAndWarnsOnOutOfRangeIndex(t *testing.T) {
	chat := &fakeChatter{dedupResponse: "[5]"}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	citations := []factCitation{
		{Fact: "claim A", URL: "https://a.example"},
		{Fact: "claim B", URL: "https://a.example"},
	}
	groups, warnings := pipeline.deduplicate(context.Background(), citations)
	if got := groups["https://a.example"].Facts; len(got) != 2 {
		t.Fatalf("facts = %v, want both facts kept when the dedup response is out of range", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning", warnings)
	}
}

func TestFactPipelineDeduplicateKeepsAllFactsAndWarnsOnDuplicateIndex(t *testing.T) {
	chat := &fakeChatter{dedupResponse: "[1,1]"}
	pipeline := &factPipeline{chat: chat, jina: &fakeJina{}}
	citations := []factCitation{
		{Fact: "claim A", URL: "https://a.example"},
		{Fact: "claim B", URL: "https://a.example"},
	}
	groups, warnings := pipeline.deduplicate(context.Background(), citations)
	if got := groups["https://a.example"].Facts; len(got) != 2 {
		t.Fatalf("facts = %v, want both facts kept when the dedup response repeats an index", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one warning", warnings)
	}
}

func TestAggregateFactReportCountsSupportedAndExcludesUnknown(t *testing.T) {
	results := map[string][]factCitationResult{
		"a": {factSupported, factUnsupported, factUnknown},
		"b": {factSupported, factSupported},
	}
	report := aggregateFactReport(results)
	if report.TotalCitations != 4 {
		t.Fatalf("TotalCitations = %d, want 4 (unknowns excluded)", report.TotalCitations)
	}
	if report.EffectiveCitations != 3 {
		t.Fatalf("EffectiveCitations = %v, want 3", report.EffectiveCitations)
	}
}

func TestRemoveURLsStripsMarkdownLinkSyntax(t *testing.T) {
	got := removeURLs("according to [ChinaFile](https://example.com/report)'s classification")
	want := "according to ChinaFile's classification"
	if got != want {
		t.Fatalf("removeURLs() = %q, want %q", got, want)
	}
}

func TestCleanURLsStripsHighlightFragment(t *testing.T) {
	got := cleanURLs("https://example.com/report#:~:text=some%20highlighted%20text")
	want := "https://example.com/report"
	if got != want {
		t.Fatalf("cleanURLs() = %q, want %q", got, want)
	}
}
