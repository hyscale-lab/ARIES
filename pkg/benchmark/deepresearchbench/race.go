package deepresearchbench

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// maxRaceRetries mirrors upstream's retry budget for a judge response that
// fails to parse as JSON or is missing an expected dimension key.
const maxRaceRetries = 10

// raceRetryDelay is the pause between retries, matching jina.go's
// jinaRetryDelay convention.
const raceRetryDelay = time.Second

// raceDimension enumerates the four RACE dimensions in a fixed, stable
// order used for prompt construction and metric key naming.
type raceDimension string

const (
	dimComprehensiveness    raceDimension = "comprehensiveness"
	dimInsight              raceDimension = "insight"
	dimInstructionFollowing raceDimension = "instruction_following"
	dimReadability          raceDimension = "readability"
)

var raceDimensions = []raceDimension{dimComprehensiveness, dimInsight, dimInstructionFollowing, dimReadability}

// criteriaFor returns dc's criterion list for dimension.
func (dc dimensionCriteria) criteriaFor(dimension raceDimension) []criterion {
	switch dimension {
	case dimComprehensiveness:
		return dc.Comprehensiveness
	case dimInsight:
		return dc.Insight
	case dimInstructionFollowing:
		return dc.InstructionFollowing
	case dimReadability:
		return dc.Readability
	default:
		return nil
	}
}

// weightFor returns dw's top-level weight for dimension.
func (dw dimensionWeights) weightFor(dimension raceDimension) float64 {
	switch dimension {
	case dimComprehensiveness:
		return dw.Comprehensiveness
	case dimInsight:
		return dw.Insight
	case dimInstructionFollowing:
		return dw.InstructionFollowing
	case dimReadability:
		return dw.Readability
	default:
		return 0
	}
}

// scoresFor returns output's returned criterion scores for dimension.
func (output raceJudgeOutput) scoresFor(dimension raceDimension) []raceCriterionScore {
	switch dimension {
	case dimComprehensiveness:
		return output.Comprehensiveness
	case dimInsight:
		return output.Insight
	case dimInstructionFollowing:
		return output.InstructionFollowing
	case dimReadability:
		return output.Readability
	default:
		return nil
	}
}

// raceCriterionScore is one parsed judge-returned line item for a dimension.
type raceCriterionScore struct {
	Criterion     string  `json:"criterion"`
	Analysis      string  `json:"analysis"`
	Article1Score float64 `json:"article_1_score"`
	Article2Score float64 `json:"article_2_score"`
}

// raceJudgeOutput is the full structured response the merged-score prompt
// requires: one array per dimension.
type raceJudgeOutput struct {
	Comprehensiveness    []raceCriterionScore `json:"comprehensiveness"`
	Insight              []raceCriterionScore `json:"insight"`
	InstructionFollowing []raceCriterionScore `json:"instruction_following"`
	Readability          []raceCriterionScore `json:"readability"`
}

// raceResult is the fully-computed, normalized RACE outcome for one task.
// Overall and the per-dimension fields are target/(target+reference) ratios
// in [0, 1]; 0.5 means the candidate tied the reference article.
type raceResult struct {
	Overall              float64
	Comprehensiveness    float64
	Insight              float64
	InstructionFollowing float64
	Readability          float64
	Raw                  raceJudgeOutput
	Warnings             []string
}

// raceScorer is the interface Evaluate depends on for RACE, swappable with a
// fake in tests.
type raceScorer interface {
	Score(ctx context.Context, prompt, targetArticle, referenceArticle string, criteria taskCriteria) (raceResult, error)
}

// raceClient scores candidate reports against a reference article using the
// upstream RACE merged-score prompt and weighted-criteria algorithm.
type raceClient struct {
	chat chatter
	// retryDelay is the pause between retries. Left at its zero value, a
	// directly-constructed raceClient (as in tests) retries with no delay;
	// newRaceClient sets it to raceRetryDelay for real judge calls.
	retryDelay time.Duration
}

var _ raceScorer = (*raceClient)(nil)

func newRaceClient(model core.ModelConfig, apiKeyLookup func(string) ([]byte, bool)) (*raceClient, error) {
	client, err := newJudgeClient(model, apiKeyLookup)
	if err != nil {
		return nil, err
	}
	return &raceClient{chat: client, retryDelay: raceRetryDelay}, nil
}

const raceSystemPrompt = "You are a strict, meticulous, and objective research article evaluation expert. " +
	"You excel at using specific assessment criteria to deeply compare two articles on the same task, providing precise scores and clear justifications."

// promptCriterion is a criteria_list entry with its weight stripped, exactly
// as upstream's format_criteria_list presents criteria to the judge (the
// judge scores relative importance implicitly via the criteria list order
// and explanations, not by seeing numeric weights).
type promptCriterion struct {
	Criterion   string `json:"criterion"`
	Explanation string `json:"explanation"`
}

type promptCriteriaList struct {
	Comprehensiveness    []promptCriterion `json:"comprehensiveness"`
	Insight              []promptCriterion `json:"insight"`
	InstructionFollowing []promptCriterion `json:"instruction_following"`
	Readability          []promptCriterion `json:"readability"`
}

func stripWeights(criteria []criterion) []promptCriterion {
	stripped := make([]promptCriterion, 0, len(criteria))
	for _, c := range criteria {
		stripped = append(stripped, promptCriterion{Criterion: c.Criterion, Explanation: c.Explanation})
	}
	return stripped
}

// buildMergedScorePrompt renders the system+user prompt pair matching
// upstream's generate_merged_score_prompt: the user prompt embeds the
// research prompt, article_1 (target, already cleaned by
// stripCitationMarkers), article_2 (reference, untouched), and a
// criteria_list JSON grouped by the four dimensions with weights stripped.
func buildMergedScorePrompt(prompt, article1, article2 string, criteria dimensionCriteria) (system, user string) {
	criteriaList := promptCriteriaList{
		Comprehensiveness:    stripWeights(criteria.Comprehensiveness),
		Insight:              stripWeights(criteria.Insight),
		InstructionFollowing: stripWeights(criteria.InstructionFollowing),
		Readability:          stripWeights(criteria.Readability),
	}
	criteriaJSON, err := json.MarshalIndent(criteriaList, "", "  ")
	if err != nil {
		// stripWeights/promptCriteriaList are plain data with no cycles or
		// unsupported types; MarshalIndent cannot fail here in practice.
		criteriaJSON = []byte("{}")
	}

	var builder strings.Builder
	builder.WriteString("<user_prompt>\n")
	builder.WriteString("**Task Background**\n")
	builder.WriteString("There is a deep research task, and you need to evaluate two research articles written for this task. ")
	builder.WriteString("We will assess the articles across four dimensions: Comprehensiveness, Insight, Instruction Following, and Readability. The content is as follows:\n")
	builder.WriteString("<task>\n\"")
	builder.WriteString(prompt)
	builder.WriteString("\"\n</task>\n\n")
	builder.WriteString("**Articles to Evaluate**\n<article_1>\n\"")
	builder.WriteString(article1)
	builder.WriteString("\"\n</article_1>\n\n<article_2>\n\"")
	builder.WriteString(article2)
	builder.WriteString("\"\n</article_2>\n\n")
	builder.WriteString("**Evaluation Criteria**\n")
	builder.WriteString("Now, you need to evaluate and compare these two articles based on the following **evaluation criteria list**, ")
	builder.WriteString("providing comparative analysis and scoring each on a scale of 0-10. Each criterion includes an explanation, please understand carefully.\n\n")
	builder.WriteString("<criteria_list>\n")
	builder.Write(criteriaJSON)
	builder.WriteString("\n</criteria_list>\n\n")
	builder.WriteString("<Instruction>\n**Your Task**\n")
	builder.WriteString("Please strictly evaluate and compare `<article_1>` and `<article_2>` based on **each criterion** in the `<criteria_list>`. You need to:\n")
	builder.WriteString("1.  **Analyze Each Criterion**: Consider how each article fulfills the requirements of each criterion.\n")
	builder.WriteString("2.  **Comparative Evaluation**: Analyze how the two articles perform on each criterion, referencing the content and criterion explanation.\n")
	builder.WriteString("3.  **Score Separately**: Based on your comparative analysis, score each article on each criterion (0-10 points).\n\n")
	builder.WriteString("**Scoring Rules**\n")
	builder.WriteString("For each criterion, score both articles on a scale of 0-10 (continuous values). The score should reflect the quality of performance on that criterion:\n")
	builder.WriteString("*   0-2 points: Very poor performance. Almost completely fails to meet the criterion requirements.\n")
	builder.WriteString("*   2-4 points: Poor performance. Minimally meets the criterion requirements with significant deficiencies.\n")
	builder.WriteString("*   4-6 points: Average performance. Basically meets the criterion requirements, neither good nor bad.\n")
	builder.WriteString("*   6-8 points: Good performance. Largely meets the criterion requirements with notable strengths.\n")
	builder.WriteString("*   8-10 points: Excellent/outstanding performance. Fully meets or exceeds the criterion requirements.\n\n")
	builder.WriteString("**Output Format Requirements**\n")
	builder.WriteString("Please **strictly** follow the `<output_format>` below for each criterion evaluation. ")
	builder.WriteString("**Do not include any other unrelated content, introduction, or summary**.\n</Instruction>\n\n")
	builder.WriteString("<output_format>\n")
	builder.WriteString(`{"comprehensiveness": [{"criterion": "...", "analysis": "...", "article_1_score": 0, "article_2_score": 0}, ...], ` +
		`"insight": [...], "instruction_following": [...], "readability": [...]}`)
	builder.WriteString("\n</output_format>\n\n")
	builder.WriteString("Now, please evaluate the two articles based on the research task and criteria, providing detailed comparative analysis and scores according to the requirements above. ")
	builder.WriteString("Ensure your output follows the specified `<output_format>` and that the JSON format is parsable, with all characters that might cause JSON parsing errors properly escaped.\n</user_prompt>")

	return raceSystemPrompt, builder.String()
}

// parseRaceJudgeOutput strictly decodes the judge's response and validates
// that all four dimensions are present and non-empty.
func parseRaceJudgeOutput(content string) (raceJudgeOutput, error) {
	var output raceJudgeOutput
	if err := json.Unmarshal([]byte(stripJSONCodeFence(content)), &output); err != nil {
		return raceJudgeOutput{}, fmt.Errorf("race judge response is not valid JSON: %w", err)
	}
	for _, dimension := range raceDimensions {
		if len(output.scoresFor(dimension)) == 0 {
			return raceJudgeOutput{}, fmt.Errorf("race judge response is missing scores for dimension %q", dimension)
		}
	}
	return output, nil
}

// Score renders the merged comparative prompt, calls the judge, and reduces
// its structured response to a normalized raceResult, retrying on malformed
// or incomplete responses.
func (c *raceClient) Score(ctx context.Context, prompt, targetArticle, referenceArticle string, criteria taskCriteria) (raceResult, error) {
	cleanedTarget := stripCitationMarkers(targetArticle)
	system, user := buildMergedScorePrompt(prompt, cleanedTarget, referenceArticle, criteria.Criterions)

	var lastErr error
	for attempt := 0; attempt < maxRaceRetries; attempt++ {
		if attempt > 0 && c.retryDelay > 0 {
			select {
			case <-ctx.Done():
				return raceResult{}, ctx.Err()
			case <-time.After(c.retryDelay):
			}
		}
		content, err := c.chat.Chat(ctx, system, user)
		if err != nil {
			lastErr = err
			continue
		}
		output, err := parseRaceJudgeOutput(content)
		if err != nil {
			lastErr = err
			continue
		}
		return calculateWeightedScores(output, criteria), nil
	}
	return raceResult{}, fmt.Errorf("race judge failed after %d attempts: %w", maxRaceRetries, lastErr)
}

// matchCriterionWeight replicates upstream's calculate_weighted_scores
// fallback chain for matching a judge-returned criterion string back to its
// vendored weight: exact match, then case-insensitive match, then substring
// match (either direction), then the arithmetic mean of the dimension's
// weights. A non-empty warning is returned whenever a fallback was used, so
// callers can surface it without this pure function needing a logger.
func matchCriterionWeight(name string, criteria []criterion) (weight float64, warning string) {
	name = strings.TrimSpace(name)
	for _, c := range criteria {
		if c.Criterion == name {
			return c.Weight, ""
		}
	}
	lower := strings.ToLower(name)
	for _, c := range criteria {
		if strings.ToLower(c.Criterion) == lower {
			return c.Weight, ""
		}
	}
	for _, c := range criteria {
		criterionLower := strings.ToLower(c.Criterion)
		if criterionLower == "" || lower == "" {
			continue
		}
		if strings.Contains(lower, criterionLower) || strings.Contains(criterionLower, lower) {
			return c.Weight, ""
		}
	}
	var sum float64
	for _, c := range criteria {
		sum += c.Weight
	}
	average := 0.0
	if len(criteria) > 0 {
		average = sum / float64(len(criteria))
	}
	return average, fmt.Sprintf("criterion %q did not match any vendored criterion; using dimension average weight %.4f", name, average)
}

// weightedDimensionAverage computes Σ(score*weight)/Σ(weight) for one side
// (target uses Article1Score, reference uses Article2Score) of one
// dimension's returned criterion scores against its vendored criteria list.
func weightedDimensionAverage(scores []raceCriterionScore, criteria []criterion, useTarget bool) (avg float64, warnings []string) {
	var weightedSum, totalWeight float64
	for _, score := range scores {
		weight, warning := matchCriterionWeight(score.Criterion, criteria)
		if warning != "" {
			warnings = append(warnings, warning)
		}
		value := score.Article2Score
		if useTarget {
			value = score.Article1Score
		}
		weightedSum += value * weight
		totalWeight += weight
	}
	if totalWeight == 0 {
		return 0, warnings
	}
	return weightedSum / totalWeight, warnings
}

// calculateWeightedScores replicates upstream's calculate_weighted_scores:
// per-dimension criterion-weight matching, per-dimension target/reference
// weighted averages, dimension_weight-combined totals, and the final
// target/(target+reference) normalization for the overall score and each
// individual dimension.
func calculateWeightedScores(output raceJudgeOutput, criteria taskCriteria) raceResult {
	result := raceResult{Raw: output}
	dimensionTarget := make(map[raceDimension]float64, len(raceDimensions))
	dimensionReference := make(map[raceDimension]float64, len(raceDimensions))

	var totalTarget, totalReference float64
	for _, dimension := range raceDimensions {
		scores := output.scoresFor(dimension)
		dimCriteria := criteria.Criterions.criteriaFor(dimension)

		targetAvg, targetWarnings := weightedDimensionAverage(scores, dimCriteria, true)
		// The reference pass runs matchCriterionWeight over the same scores
		// and criteria as the target pass, so its warnings are identical;
		// only the target-side warnings are kept to avoid duplicates.
		referenceAvg, _ := weightedDimensionAverage(scores, dimCriteria, false)
		result.Warnings = append(result.Warnings, targetWarnings...)

		dimensionTarget[dimension] = targetAvg
		dimensionReference[dimension] = referenceAvg

		dimWeight := criteria.Weights.weightFor(dimension)
		totalTarget += targetAvg * dimWeight
		totalReference += referenceAvg * dimWeight
	}

	result.Overall = normalizeRatio(totalTarget, totalReference)
	result.Comprehensiveness = normalizeRatio(dimensionTarget[dimComprehensiveness], dimensionReference[dimComprehensiveness])
	result.Insight = normalizeRatio(dimensionTarget[dimInsight], dimensionReference[dimInsight])
	result.InstructionFollowing = normalizeRatio(dimensionTarget[dimInstructionFollowing], dimensionReference[dimInstructionFollowing])
	result.Readability = normalizeRatio(dimensionTarget[dimReadability], dimensionReference[dimReadability])
	return result
}

// normalizeRatio computes target/(target+reference), matching upstream's
// RACE normalization; returns 0 when both sides are 0 to avoid dividing by
// zero (an all-zero comparison has no meaningful ratio).
func normalizeRatio(target, reference float64) float64 {
	if target+reference == 0 {
		return 0
	}
	return target / (target + reference)
}
