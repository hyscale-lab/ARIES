package deepresearchbench

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const factMaxRetries = 3

// factRetryDelay is the pause between validateGroup retries, matching
// jina.go's jinaRetryDelay convention.
const factRetryDelay = time.Second

// factCitation is one extracted (fact, ref_idx, url) triplet before dedup.
// ref_idx is intentionally not decoded: the judge returns it inconsistently
// typed (string or number) across providers, and ARIES's dedup/validate
// pipeline groups by URL rather than ref_idx, so the field is never read.
type factCitation struct {
	Fact string `json:"fact"`
	URL  string `json:"url"`
}

// factURLGroup is the post-dedup, pre-validation state for one unique URL.
type factURLGroup struct {
	Facts      []string
	URLContent *string // nil until scraped; a non-nil "scrape failed: ..." string still counts as attempted
}

// factCitationResult is the outcome for one deduplicated fact after
// validation, mirroring upstream's three-way supported/unsupported/unknown
// classification.
type factCitationResult string

const (
	factSupported   factCitationResult = "supported"
	factUnsupported factCitationResult = "unsupported"
	factUnknown     factCitationResult = "unknown"
)

// factReport is the fully-computed per-task FACT outcome.
type factReport struct {
	// EffectiveCitations is the count of citations judged supported.
	EffectiveCitations float64 `json:"effective_citations"`
	// TotalCitations is the count of non-unknown citations (the denominator
	// for citation accuracy).
	TotalCitations int `json:"total_citations"`
	// HasCitations is false when the article had no extractable in-text
	// citations at all; callers should exclude such tasks from
	// cross-task averages rather than recording a misleading 0.
	HasCitations bool     `json:"has_citations"`
	Warnings     []string `json:"warnings,omitempty"`
}

// factRunner is the interface Evaluate depends on for FACT, swappable with
// a fake in tests. A nil factRunner on Benchmark means FACT is disabled.
type factRunner interface {
	Run(ctx context.Context, article string) (factReport, error)
}

// factPipeline is the host-side FACT orchestrator: extract -> deduplicate ->
// scrape -> validate -> aggregate, run against the raw, uncleaned candidate
// article (citation markers must stay intact for extraction).
type factPipeline struct {
	chat chatter
	jina jinaScraper
	// validateRetryDelay is the pause between validateGroup retries. Left at
	// its zero value, a directly-constructed factPipeline (as in tests)
	// retries with no delay; newFactPipeline sets it to factRetryDelay.
	validateRetryDelay time.Duration
}

var _ factRunner = (*factPipeline)(nil)

func newFactPipeline(model core.ModelConfig, apiKeyLookup func(string) ([]byte, bool), jinaAPIKey []byte) (*factPipeline, error) {
	chat, err := newJudgeClient(model, apiKeyLookup)
	if err != nil {
		return nil, err
	}
	return &factPipeline{chat: chat, jina: newJinaClient(jinaAPIKey), validateRetryDelay: factRetryDelay}, nil
}

// Run executes the full FACT pipeline against one raw candidate article.
func (p *factPipeline) Run(ctx context.Context, article string) (factReport, error) {
	citations, err := p.extract(ctx, article)
	if err != nil {
		return factReport{}, fmt.Errorf("fact extract: %w", err)
	}
	if len(citations) == 0 {
		return factReport{HasCitations: false}, nil
	}

	groups, dedupWarnings := p.deduplicate(ctx, citations)

	if err := p.scrape(ctx, groups); err != nil {
		return factReport{}, fmt.Errorf("fact scrape: %w", err)
	}

	results, validateWarnings, err := p.validate(ctx, groups)
	if err != nil {
		return factReport{}, fmt.Errorf("fact validate: %w", err)
	}

	report := aggregateFactReport(results)
	report.HasCitations = true
	report.Warnings = append(dedupWarnings, validateWarnings...)
	return report, nil
}

const factExtractPrompt = `You will be provided with a research report. The body of the report will contain some citations to references.

Citations in the main text may appear in the following forms:
1. A segment of text + space + number, for example: "Li Qiang constructed a socioeconomic status index (SES) based on income, education, and occupation, dividing society into 7 levels 15"
2. A segment of text + [number], for example: "Li Qiang constructed a socioeconomic status index (SES) based on income, education, and occupation, dividing society into 7 levels[15]"
3. A segment of text + [number†(some line numbers, etc.)], for example: "Li Qiang constructed a socioeconomic status index (SES) based on income, education, and occupation, dividing society into 7 levels[15†L10][5L23][7†summary]"
4. [Citation Source](Citation Link), for example: "According to [ChinaFile: A Guide to Social Class in Modern China](https://www.chinafile.com/reporting-opinion/media/guide-social-class-modern-china)'s classification, Chinese society can be divided into nine strata"

Please identify **all** instances where references are cited in the main text, and extract (fact, ref_idx, url) triplets. When extracting, pay attention to the following:
1. Since these facts will need to be verified later, you may need to look for some context before and after the citation to ensure that the fact is complete and understandable, rather than just a simple phrase or short expression.
2. If a fact cites multiple references, then it should correspond to two triplets: (fact, ref_idx_1, url_1) and (fact, ref_idx_2, url_2).
3. For the third form of citation (i.e., where the citation source and link appear directly in the text), the ref_idx should be uniformly set to 0.
4. If the main text does not specify the exact location of the citation (for example, only the reference list is listed at the end of the article, without specifying the citation point in the text), please return an empty list.

You should return a JSON list format, where each item in the list is a triplet, for example:
[
    {
        "fact": "Text segment from the original document.",
        "ref_idx": "The index of the cited reference in the reference list for this text segment.",
        "url": "The URL of the cited reference for this text segment (extracted from the reference list at the end of the research report or from the parentheses at the citation point)."
    }
]

Here is the main text of the research report:
%s

Please begin the extraction now. Output only the JSON list directly, without any chitchat or explanations.`

// markdownLinkPattern matches "[title](url)" markdown link syntax.
var markdownLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// urlFragmentPattern strips a "#:~:text=" highlight-fragment suffix some
// search engines (and OpenAI's own citations) append to URLs.
var urlFragmentPattern = regexp.MustCompile(`#:~:text=.*$`)

// cleanURLs strips "#:~:text=" highlight fragments from a URL.
func cleanURLs(rawURL string) string {
	return urlFragmentPattern.ReplaceAllString(rawURL, "")
}

// removeURLs strips markdown "[title](url)" link syntax from an extracted
// fact string, leaving only the visible title text. This is FACT-only
// cleanup applied to extracted fact strings, distinct from clean.go's
// stripCitationMarkers (which operates on the whole article for RACE).
func removeURLs(fact string) string {
	return markdownLinkPattern.ReplaceAllString(fact, "$1")
}

func (p *factPipeline) extract(ctx context.Context, article string) ([]factCitation, error) {
	prompt := fmt.Sprintf(factExtractPrompt, article)
	content, err := p.chat.Chat(ctx, "", prompt)
	if err != nil {
		return nil, err
	}
	var citations []factCitation
	if err := json.Unmarshal([]byte(stripJSONCodeFence(content)), &citations); err != nil {
		return nil, fmt.Errorf("parse extracted citations: %w", err)
	}
	for i := range citations {
		citations[i].URL = cleanURLs(citations[i].URL)
		citations[i].Fact = removeURLs(citations[i].Fact)
	}
	return citations, nil
}

const factDedupPrompt = `You will be given a list of statements. You need to de-duplicate them and return a list of indices of the unique statements. Note: Two statements are considered duplicates only if they express *exactly the same thing*. If there are no duplicate statements in the list, return the complete list of indices.

You should return a List(int), where each item in the list is the index of a unique, non-duplicated statement that has been retained. For example:
[1, 3, 5]

Below is the list of statements you need to de-duplicate:
%s

Please begin the extraction now. Output only the integer list, without any conversational text or explanations.`

// deduplicate groups citations by URL. Groups with more than one fact are
// deduplicated via one cheap LLM call each, asking which indices express
// genuinely distinct claims; single-fact groups skip the call entirely. A
// group whose dedup call fails or returns an unusable response keeps all its
// facts rather than failing the whole pipeline over a cosmetic near-duplicate
// merge, and records why in the returned warnings. Dedup can never fail the
// pipeline outright, so it returns no error.
func (p *factPipeline) deduplicate(ctx context.Context, citations []factCitation) (map[string]*factURLGroup, []string) {
	byURL := make(map[string][]string)
	order := make([]string, 0)
	for _, c := range citations {
		url := strings.TrimSpace(c.URL)
		if url == "" {
			continue
		}
		if _, seen := byURL[url]; !seen {
			order = append(order, url)
		}
		byURL[url] = append(byURL[url], c.Fact)
	}

	var warnings []string
	groups := make(map[string]*factURLGroup, len(order))
	for _, url := range order {
		facts := byURL[url]
		if len(facts) == 1 {
			groups[url] = &factURLGroup{Facts: facts}
			continue
		}
		deduped, warning := p.deduplicateFacts(ctx, facts)
		if warning != "" {
			warnings = append(warnings, fmt.Sprintf("deduplicate %q: %s", url, warning))
		}
		groups[url] = &factURLGroup{Facts: deduped}
	}
	return groups, warnings
}

// deduplicateFacts asks the LLM which facts among a same-URL group express
// genuinely distinct claims. Any failure to get a usable answer — a failed
// call, an unparseable response, or an out-of-range index — degrades to
// "keep everything" rather than failing the whole pipeline over a cosmetic
// near-duplicate merge; the second return value explains why when that
// happens, and is empty on a clean dedup.
func (p *factPipeline) deduplicateFacts(ctx context.Context, facts []string) ([]string, string) {
	var statements strings.Builder
	for i, fact := range facts {
		fmt.Fprintf(&statements, "%d. %s\n", i+1, fact)
	}
	prompt := fmt.Sprintf(factDedupPrompt, statements.String())

	content, err := p.chat.Chat(ctx, "", prompt)
	if err != nil {
		return facts, fmt.Sprintf("dedup call failed, keeping all facts: %v", err)
	}
	var indices []int
	if err := json.Unmarshal([]byte(stripJSONCodeFence(content)), &indices); err != nil {
		return facts, fmt.Sprintf("dedup response unparseable, keeping all facts: %v", err)
	}
	if len(indices) == 0 || len(indices) > len(facts) {
		return facts, fmt.Sprintf("dedup response had %d indices for %d facts, keeping all facts", len(indices), len(facts))
	}
	seen := make(map[int]struct{}, len(indices))
	kept := make([]string, 0, len(indices))
	for _, index := range indices {
		if index < 1 || index > len(facts) {
			return facts, fmt.Sprintf("dedup response referenced out-of-range index %d, keeping all facts", index)
		}
		if _, duplicate := seen[index]; duplicate {
			return facts, fmt.Sprintf("dedup response referenced index %d more than once, keeping all facts", index)
		}
		seen[index] = struct{}{}
		kept = append(kept, facts[index-1])
	}
	return kept, ""
}

// scrape fetches every unique URL's content concurrently. A failed fetch
// still produces a non-nil "scrape failed: <error>" content string rather
// than leaving it nil, matching upstream's scrape.py semantics: that string
// is deliberately still handed to validation, which resolves it to
// unsupported/unknown naturally rather than the pipeline special-casing it.
func (p *factPipeline) scrape(ctx context.Context, groups map[string]*factURLGroup) error {
	const maxConcurrentScrapes = 8
	semaphore := make(chan struct{}, maxConcurrentScrapes)
	var wg sync.WaitGroup
	for url, group := range groups {
		wg.Add(1)
		go func(url string, group *factURLGroup) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			content, err := p.jina.Fetch(ctx, url)
			if err != nil {
				failed := fmt.Sprintf("scrape failed: %v", err)
				group.URLContent = &failed
				return
			}
			group.URLContent = &content
		}(url, group)
	}
	wg.Wait()
	return nil
}

const factValidatePrompt = `You will be provided with a reference and some statements. Please determine whether each statement is 'supported', 'unsupported', or 'unknown' with respect to the reference. Please note:
First, assess whether the reference contains any valid content. If the reference contains no valid information, such as a 'page not found' message, then all statements should be considered 'unknown'.
If the reference is valid, for a given statement: if the facts or data it contains can be found entirely or partially within the reference, it is considered 'supported' (data accepts rounding); if all facts and data in the statement cannot be found in the reference, it is considered 'unsupported'.

You should return the result in a JSON list format, where each item in the list contains the statement's index and the judgment result, for example:
[
    {
        "idx": 1,
        "result": "supported"
    },
    {
        "idx": 2,
        "result": "unsupported"
    }
]

Below are the reference and statements:
<reference>
%s
</reference>

<statements>
%s
</statements>

Begin the assessment now. Output only the JSON list, without any conversational text or explanations.`

type factValidationEntry struct {
	Idx    int    `json:"idx"`
	Result string `json:"result"`
}

// validate classifies every fact in every group as supported/unsupported/
// unknown. Groups with nil content (never scraped) get an all-unknown
// result without an LLM call, mirroring upstream's "no reference" path.
func (p *factPipeline) validate(ctx context.Context, groups map[string]*factURLGroup) (map[string][]factCitationResult, []string, error) {
	results := make(map[string][]factCitationResult, len(groups))
	var warnings []string
	for url, group := range groups {
		if group.URLContent == nil {
			unknown := make([]factCitationResult, len(group.Facts))
			for i := range unknown {
				unknown[i] = factUnknown
			}
			results[url] = unknown
			continue
		}
		classified, err := p.validateGroup(ctx, *group.URLContent, group.Facts)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("validate %q: %v", url, err))
			unknown := make([]factCitationResult, len(group.Facts))
			for i := range unknown {
				unknown[i] = factUnknown
			}
			results[url] = unknown
			continue
		}
		results[url] = classified
	}
	return results, warnings, nil
}

func (p *factPipeline) validateGroup(ctx context.Context, reference string, facts []string) ([]factCitationResult, error) {
	var statements strings.Builder
	for i, fact := range facts {
		fmt.Fprintf(&statements, "%d. %s\n", i+1, fact)
	}
	prompt := fmt.Sprintf(factValidatePrompt, reference, statements.String())

	var lastErr error
	for attempt := 0; attempt < factMaxRetries; attempt++ {
		if attempt > 0 && p.validateRetryDelay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(p.validateRetryDelay):
			}
		}
		content, err := p.chat.Chat(ctx, "", prompt)
		if err != nil {
			lastErr = err
			continue
		}
		var entries []factValidationEntry
		if err := json.Unmarshal([]byte(stripJSONCodeFence(content)), &entries); err != nil || len(entries) != len(facts) {
			lastErr = fmt.Errorf("validate response has %d entries, want %d", len(entries), len(facts))
			continue
		}
		classified := make([]factCitationResult, len(facts))
		for i := range classified {
			classified[i] = factUnknown
		}
		valid := true
		for _, entry := range entries {
			index := entry.Idx - 1
			if index < 0 || index >= len(facts) {
				valid = false
				break
			}
			switch factCitationResult(entry.Result) {
			case factSupported, factUnsupported, factUnknown:
				classified[index] = factCitationResult(entry.Result)
			default:
				classified[index] = factUnknown
			}
		}
		if !valid {
			lastErr = fmt.Errorf("validate response index out of range")
			continue
		}
		return classified, nil
	}
	return nil, lastErr
}

// aggregateFactReport is a pure function reducing every URL group's
// per-fact classifications into the task-level FACT metrics: TotalCitations
// counts every non-unknown fact, EffectiveCitations counts supported facts.
func aggregateFactReport(results map[string][]factCitationResult) factReport {
	var report factReport
	for _, classifications := range results {
		for _, result := range classifications {
			if result == factUnknown {
				continue
			}
			report.TotalCitations++
			if result == factSupported {
				report.EffectiveCitations++
			}
		}
	}
	return report
}
