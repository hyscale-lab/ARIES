package sweatlas

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxRubricRetries mirrors evaluate_answer.py's MAX_RETRIES = 8.
// rubricRetryBaseDelay/rubricRetryCap implement its exponential backoff
// (wait = min(2 ** (attempt + 1), 60) seconds), applied only when the judge
// call itself fails (a network/HTTP error); an unparseable-but-successful
// response retries immediately with no delay, exactly like upstream. These
// are package-level vars, not consts, so tests can shrink them to avoid
// waiting out the real bound.
const maxRubricRetries = 8

var (
	rubricRetryBaseDelay = time.Second
	rubricRetryCap       = 60 * time.Second
)

// rubric is one entry of tests/rubrics.json. Importance is a pointer because
// upstream's rubric.get("importance", "must have") reads a top-level field
// that the vendored dataset's rubrics never actually set (see the sample
// task's rubrics.json), always falling back to "must have"; a pointer lets
// effectiveImportance distinguish "absent" from "explicitly set" the same
// way Python's dict.get default does.
type rubric struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Importance  *string `json:"importance"`
	Annotations struct {
		Type string `json:"type"`
	} `json:"annotations"`
}

func (r rubric) effectiveImportance() string {
	if r.Importance != nil {
		return *r.Importance
	}
	return "must have"
}

func loadRubrics(path string) ([]rubric, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rubrics: %w", err)
	}
	var rubrics []rubric
	if err := json.Unmarshal(content, &rubrics); err != nil {
		return nil, fmt.Errorf("parse rubrics %q: %w", path, err)
	}
	return rubrics, nil
}

// numericPrefix matches a leading "1.1: "-style prefix on a rubric title,
// mirroring evaluate_answer.py's re.sub(r"^\d+(\.\d+)*:\s*", "", title).
var numericPrefix = regexp.MustCompile(`^\d+(\.\d+)*:\s*`)

func stripNumericPrefix(title string) string {
	return numericPrefix.ReplaceAllString(title, "")
}

// renderUserPrompt substitutes user_prompt_template.txt's three Python
// str.format() placeholders literally (the template has no other format
// directives to support), then unescapes the template's "{{"/"}}" literal
// braces exactly as str.format() would.
func renderUserPrompt(template, problemStatement, modelAnswer, title string) string {
	rendered := strings.ReplaceAll(template, "{problem_statement}", problemStatement)
	rendered = strings.ReplaceAll(rendered, "{model_answer}", modelAnswer)
	rendered = strings.ReplaceAll(rendered, "{title}", title)
	rendered = strings.ReplaceAll(rendered, "{{", "{")
	rendered = strings.ReplaceAll(rendered, "}}", "}")
	return rendered
}

// rawRating is one ratings[] entry as decoded straight off the judge's JSON
// response, before any status/score normalization. Fields are `any` because
// the judge may return a JSON number (e.g. score: 1) or a JSON string
// (score: "1") interchangeably; normalizeStatus/normalizeScore stringify
// either the same way Python's str(value) does.
type rawRating struct {
	RubricStatement any `json:"rubric_statement"`
	Status          any `json:"status"`
	Score           any `json:"score"`
	Justification   any `json:"justification"`
}

type ratingsResponse struct {
	Ratings []rawRating `json:"ratings"`
}

// parseJudgeResponse mirrors evaluate_answer.py's _parse_response: strip a
// ```json fence if present, else look for a {"ratings"...} object by
// substring search and brace-matching, then decode and return the first
// ratings[] entry.
func parseJudgeResponse(content string) (*rawRating, bool) {
	text := strings.TrimSpace(content)
	if text == "" {
		return nil, false
	}

	if strings.Contains(text, "```json") {
		after := text[strings.Index(text, "```json")+len("```json"):]
		if end := strings.Index(after, "```"); end != -1 {
			text = strings.TrimSpace(after[:end])
		}
	}

	if !strings.HasPrefix(text, "{") {
		start := strings.Index(text, `{"ratings"`)
		if start == -1 {
			start = strings.Index(text, `{ "ratings"`)
		}
		if start != -1 {
			text = text[start:]
			depth := 0
			for index, character := range text {
				switch character {
				case '{':
					depth++
				case '}':
					depth--
				}
				if depth == 0 {
					text = text[:index+1]
					break
				}
			}
		}
	}

	var decoded ratingsResponse
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, false
	}
	if len(decoded.Ratings) == 0 {
		return nil, false
	}
	return &decoded.Ratings[0], true
}

// normalizeStatus mirrors evaluate_answer.py's _normalize_status.
func normalizeStatus(value any) string {
	if value == nil {
		return ""
	}
	status := strings.ToUpper(strings.TrimSpace(stringifyJudgeValue(value)))
	switch status {
	case "YES", "Y", "TRUE", "1":
		return "YES"
	case "NO", "N", "FALSE", "0":
		return "NO"
	default:
		return ""
	}
}

// normalizeScore mirrors evaluate_answer.py's _normalize_score.
func normalizeScore(value any) string {
	if value == nil {
		return ""
	}
	score := strings.TrimSpace(stringifyJudgeValue(value))
	switch score {
	case "1", "1.0":
		return "1"
	case "0", "0.0":
		return "0"
	}
	switch strings.ToLower(score) {
	case "yes", "true":
		return "1"
	case "no", "false":
		return "0"
	default:
		return ""
	}
}

// stringifyJudgeValue mirrors Python's str(value) for the JSON scalar types
// a judge response can realistically contain (string, float64, bool).
func stringifyJudgeValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		if typed {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// scoreFromStatus mirrors evaluate_answer.py's _score_from_status.
func scoreFromStatus(status string) string {
	switch status {
	case "YES":
		return "1"
	case "NO":
		return "0"
	default:
		return ""
	}
}

// applyNegativeFlip mirrors evaluate_answer.py's _apply_negative_flip.
func applyNegativeFlip(rawScore, rubricType string) (score string, flipped bool) {
	if rawScore != "0" && rawScore != "1" {
		return "", false
	}
	if strings.Contains(strings.ToLower(rubricType), "negative") {
		if rawScore == "1" {
			return "0", true
		}
		return "1", true
	}
	return rawScore, false
}

// judgeScore is the raw {rubric_statement, status, score, justification}
// tuple pulled straight off the judge's first ratings[] entry, before
// canonicalization — evaluate_answer.py's local judge_score dict.
type judgeScore struct {
	RubricStatement any `json:"rubric_statement"`
	Status          any `json:"status"`
	Score           any `json:"score"`
	Justification   any `json:"justification"`
}

// canonicalResult mirrors evaluate_answer.py's _canonicalize_judge_result
// output shape, written into evaluation_results.json's per-rubric
// "score" field.
type canonicalResult struct {
	RubricStatement          any        `json:"rubric_statement"`
	Status                   string     `json:"status"`
	Score                    string     `json:"score"`
	Justification            any        `json:"justification"`
	JudgeScore               judgeScore `json:"judge_score"`
	JudgeScoreCanonical      string     `json:"judge_score_canonical"`
	JudgeStatusScoreMismatch bool       `json:"judge_status_score_mismatch"`
	WasFlipped               bool       `json:"was_flipped"`
	RubricType               string     `json:"rubric_type"`
}

func (c *canonicalResult) isScored() bool {
	return c != nil && (c.Score == "0" || c.Score == "1")
}

// canonicalizeJudgeResult mirrors evaluate_answer.py's
// _canonicalize_judge_result.
func canonicalizeJudgeResult(parsed rawRating, rubricType string) *canonicalResult {
	score := judgeScore{
		RubricStatement: parsed.RubricStatement,
		Status:          parsed.Status,
		Score:           parsed.Score,
		Justification:   parsed.Justification,
	}
	normalizedStatus := normalizeStatus(score.Status)
	normalizedScore := normalizeScore(score.Score)
	statusScore := scoreFromStatus(normalizedStatus)

	mismatch := normalizedStatus != "" && normalizedScore != "" && statusScore != normalizedScore

	canonicalRawScore := statusScore
	if canonicalRawScore == "" {
		canonicalRawScore = normalizedScore
	}
	effectiveScore, wasFlipped := applyNegativeFlip(canonicalRawScore, rubricType)

	var effectiveStatus string
	switch {
	case effectiveScore == "0" || effectiveScore == "1":
		effectiveStatus = "YES"
		if effectiveScore == "0" {
			effectiveStatus = "NO"
		}
	case canonicalRawScore == "0" || canonicalRawScore == "1":
		effectiveStatus = "YES"
		if canonicalRawScore == "0" {
			effectiveStatus = "NO"
		}
	default:
		effectiveStatus = normalizedStatus
	}

	return &canonicalResult{
		RubricStatement:          score.RubricStatement,
		Status:                   effectiveStatus,
		Score:                    effectiveScore,
		Justification:            score.Justification,
		JudgeScore:               score,
		JudgeScoreCanonical:      canonicalRawScore,
		JudgeStatusScoreMismatch: mismatch,
		WasFlipped:               wasFlipped,
		RubricType:               rubricType,
	}
}

// evaluateSingleRubric mirrors evaluate_answer.py's evaluate_single_rubric:
// render the user prompt, call the judge, and retry until a response scores
// validly or the retry budget is exhausted. A successful call that returns
// an unparseable/invalid response retries immediately (no delay); only a
// failed call (network/HTTP error) waits with exponential backoff, exactly
// like upstream. The returned error is the last failure seen before the
// retry budget (or the context deadline) was exhausted — nil whenever a
// canonicalResult is returned — so callers can distinguish "the judge
// consistently errored" (a real fault worth surfacing) from "the judge
// responded but every response was unparseable" (still worth surfacing, but
// a different failure mode) rather than silently reporting an unscored
// rubric with no diagnostic at all.
func evaluateSingleRubric(ctx context.Context, chat chatter, systemPrompt, userPromptTemplate, problemStatement, modelAnswer string, r rubric) (*canonicalResult, error) {
	title := stripNumericPrefix(r.Title)
	titleJSON, err := json.Marshal(title)
	if err != nil {
		titleJSON = []byte(strconv.Quote(title))
	}
	userPrompt := renderUserPrompt(userPromptTemplate, problemStatement, modelAnswer, string(titleJSON))

	var lastErr error
	for attempt := 0; attempt < maxRubricRetries; attempt++ {
		content, err := chat.chat(ctx, systemPrompt, userPrompt)
		if err != nil {
			lastErr = err
			wait := rubricRetryBaseDelay * time.Duration(1<<uint(attempt+1))
			if wait > rubricRetryCap {
				wait = rubricRetryCap
			}
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("judge call failed after %d attempt(s), then context ended: %w", attempt+1, lastErr)
			case <-time.After(wait):
			}
			continue
		}

		parsed, ok := parseJudgeResponse(content)
		if ok {
			statusScore := scoreFromStatus(normalizeStatus(parsed.Status))
			parsedScore := normalizeScore(parsed.Score)
			if statusScore == "0" || statusScore == "1" || parsedScore == "0" || parsedScore == "1" {
				return canonicalizeJudgeResult(*parsed, r.Annotations.Type), nil
			}
		}
		lastErr = fmt.Errorf("judge response did not contain a valid rating: %q", truncateForError(content, 200))
	}
	return nil, fmt.Errorf("exhausted %d attempts: %w", maxRubricRetries, lastErr)
}

func truncateForError(content string, max int) string {
	if len(content) <= max {
		return content
	}
	return content[:max] + "..."
}

// rubricScore is one tests_scores entry written to evaluation_results.json,
// mirroring evaluate_answer.py's per-rubric results list entry.
type rubricScore struct {
	ID         string           `json:"id"`
	Title      string           `json:"title"`
	Importance string           `json:"importance"`
	Score      *canonicalResult `json:"score"`
}

// evaluationResults is the full evaluation_results.json shape,
// mirroring evaluate_answer.py's main() output dict.
type evaluationResults struct {
	Reward              int           `json:"reward"`
	Pass                bool          `json:"pass"`
	AggScore            float64       `json:"agg_score"`
	NumRubrics          int           `json:"num_rubrics"`
	NumScored           int           `json:"num_scored"`
	NumUnscored         int           `json:"num_unscored"`
	NumScoredMustHave   int           `json:"num_scored_must_have"`
	NumUnscoredMustHave int           `json:"num_unscored_must_have"`
	NumPassed           int           `json:"num_passed"`
	RubricScores        []rubricScore `json:"rubric_scores"`
}

// aggregateRubricResults mirrors evaluate_answer.py's main() aggregation:
// reward is 1 iff every scored "must have" rubric scored 1 (and at least one
// was scored); agg_score is the mean score over all scored rubrics of any
// importance.
func aggregateRubricResults(rubrics []rubric, scores []*canonicalResult) evaluationResults {
	results := make([]rubricScore, len(rubrics))
	for index, r := range rubrics {
		results[index] = rubricScore{ID: r.ID, Title: r.Title, Importance: r.effectiveImportance(), Score: scores[index]}
	}

	var scoredMustHave, unscoredMustHave int
	allMustHavesPass := true
	var scoredCount, passedCount int
	var scoreSum float64
	for _, result := range results {
		if result.Score.isScored() {
			scoredCount++
			scoreSum += float64(mustAtoi(result.Score.Score))
			if result.Score.Score == "1" {
				passedCount++
			}
		}
		if result.Importance == "must have" {
			if result.Score.isScored() {
				scoredMustHave++
				if result.Score.Score != "1" {
					allMustHavesPass = false
				}
			} else {
				unscoredMustHave++
			}
		}
	}
	allPass := scoredMustHave > 0 && allMustHavesPass

	aggScore := 0.0
	if scoredCount > 0 {
		aggScore = scoreSum / float64(scoredCount)
	}
	reward := 0
	if allPass {
		reward = 1
	}

	return evaluationResults{
		Reward:              reward,
		Pass:                allPass,
		AggScore:            aggScore,
		NumRubrics:          len(rubrics),
		NumScored:           scoredCount,
		NumUnscored:         len(rubrics) - scoredCount,
		NumScoredMustHave:   scoredMustHave,
		NumUnscoredMustHave: unscoredMustHave,
		NumPassed:           passedCount,
		RubricScores:        results,
	}
}

func mustAtoi(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
