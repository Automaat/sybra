package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
	"github.com/Automaat/sybra/internal/provider"
)

// defaultJudgeModel is a capable model for nuanced code-quality judgment.
const defaultJudgeModel = "claude-sonnet-4-6"

// maxDiffChars caps the diff fed to the judge so a huge PR can't blow the prompt.
const maxDiffChars = 16000

// RubricDimension is one scored axis of the mid-level-SWE quality rubric.
type RubricDimension struct {
	Key      string
	Question string
}

// Rubric is the set of dimensions a landed change is scored on (0–10 each).
var Rubric = []RubricDimension{
	{"correctness", "Does the change correctly do what the task asked, with tests passing?"},
	{"code_quality", "Is the code idiomatic, readable, and consistent with the surrounding code?"},
	{"scope_discipline", "Did it solve the task without over-reaching or leaving it half-done?"},
	{"test_coverage", "Were tests added or updated appropriately for the behavior changed?"},
	{"review_worthiness", "Would this survive a mid-level engineer's PR review without major changes?"},
}

// DimensionScore is a single 0–10 score with a one-line rationale.
type DimensionScore struct {
	Score     int    `json:"score"`
	Rationale string `json:"rationale,omitempty"`
}

// QualityVerdict is the judge's assessment of one landed change.
type QualityVerdict struct {
	TaskID     string                    `json:"taskId,omitempty"`
	Overall    float64                   `json:"overall"`
	Dimensions map[string]DimensionScore `json:"dimensions"`
	Summary    string                    `json:"summary,omitempty"`
}

// JudgeRequest is the material the judge scores: the task intent, the landed
// diff, and (optionally) a compact trajectory summary of how the agent worked.
type JudgeRequest struct {
	TaskID     string
	Title      string
	Body       string
	Diff       string
	Trajectory string
}

// QualityJudge scores a landed change against the rubric.
type QualityJudge interface {
	Judge(ctx context.Context, req JudgeRequest) (QualityVerdict, error)
}

// ClaudeQualityJudge spawns `claude -p` and parses the rubric verdict, mirroring
// selfmonitor.ClaudeJudge. DimSeed permutes the rubric order per call to reduce
// the LLM's position bias toward dimensions listed first (seed 0 = stable order).
type ClaudeQualityJudge struct {
	Model   string
	DimSeed int64
	Logger  *slog.Logger
	Gate    provider.HealthGate
}

// Judge shells out to claude -p and returns a validated verdict.
func (j *ClaudeQualityJudge) Judge(ctx context.Context, req JudgeRequest) (QualityVerdict, error) {
	model := j.Model
	if model == "" {
		model = defaultJudgeModel
	}
	prompt := buildQualityPrompt(req, shuffledRubric(j.DimSeed))
	v, _, err := llmjob.Run(ctx, prompt, llmjob.Spec[QualityVerdict]{
		Name:     "quality-judge",
		Tier:     qualityTier(model),
		Validate: validateQualityVerdict,
	}, llmexec.Options{Logger: j.Logger, Gate: j.Gate})
	if err != nil {
		return QualityVerdict{}, err
	}
	v.TaskID = req.TaskID
	return v, nil
}

func qualityTier(model string) llmjob.Tier {
	if strings.Contains(strings.ToLower(model), "opus") {
		return llmjob.Deep
	}
	return llmjob.Standard
}

// shuffledRubric returns the rubric in a deterministic permutation for the seed,
// to mitigate the LLM's position bias toward dimensions listed first. Seed 0
// leaves the canonical order untouched. The order is derived by ranking each
// dimension by a hash of "seed:key", which is reproducible for a given seed and
// avoids any signed/unsigned integer conversions.
func shuffledRubric(seed int64) []RubricDimension {
	out := append([]RubricDimension(nil), Rubric...)
	if seed == 0 {
		return out
	}
	rank := func(key string) [32]byte {
		return sha256.Sum256(fmt.Appendf(nil, "%d:%s", seed, key))
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Key), rank(out[j].Key)
		return string(ri[:]) < string(rj[:])
	})
	return out
}

func buildQualityPrompt(req JudgeRequest, dims []RubricDimension) string {
	var b strings.Builder
	b.WriteString("You are a staff engineer grading whether an AI agent's merged code change ")
	b.WriteString("meets the bar of a competent mid-level software engineer.\n")
	b.WriteString("Score each dimension 0–10 (10 = excellent). Output ONLY a single JSON object on the final line.\n\n")

	fmt.Fprintf(&b, "Task: %s\n", req.Title)
	if body := truncate(req.Body, 800); body != "" {
		fmt.Fprintf(&b, "Task details: %s\n", body)
	}
	if req.Trajectory != "" {
		fmt.Fprintf(&b, "How the agent worked: %s\n", req.Trajectory)
	}
	b.WriteString("\nDiff (may be truncated):\n")
	b.WriteString(truncate(req.Diff, maxDiffChars))
	b.WriteString("\n\nScore these dimensions:\n")
	for _, d := range dims {
		fmt.Fprintf(&b, "- %s: %s\n", d.Key, d.Question)
	}

	b.WriteString("\nOutput schema (single JSON object, nothing else):\n")
	b.WriteString(`{"dimensions":{`)
	for i, d := range dims {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s":{"score":0,"rationale":"..."}`, d.Key)
	}
	b.WriteString(`},"overall":0.0,"summary":"one sentence"}`)
	b.WriteString("\n\nRules:\n")
	b.WriteString("- Be a tough but fair reviewer; reserve 9–10 for genuinely excellent work.\n")
	b.WriteString("- overall is the mean of the dimension scores unless one is a blocker.\n")
	b.WriteString("- rationale: one concrete clause, not boilerplate.\n")
	return b.String()
}

// parseQualityVerdict extracts the verdict from `claude -p --output-format json`
// stdout, clamps each dimension score to [0,10], and fills Overall from the mean
// when the model omitted or mis-scored it.
func parseQualityVerdict(raw []byte) (QualityVerdict, error) {
	text := string(raw)
	var envelope struct {
		Result *string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != nil {
		if *envelope.Result == "" {
			return QualityVerdict{}, fmt.Errorf("empty result field")
		}
		text = *envelope.Result
	}
	jsonStr := judgeExtractLastJSON(text)
	if jsonStr == "" {
		return QualityVerdict{}, fmt.Errorf("no JSON object in result: %q", text)
	}
	var v QualityVerdict
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return QualityVerdict{}, fmt.Errorf("unmarshal verdict: %w", err)
	}
	if err := validateQualityVerdict(&v); err != nil {
		return QualityVerdict{}, err
	}
	return v, nil
}

func validateQualityVerdict(v *QualityVerdict) error {
	// Require every rubric dimension: a partial verdict would skew Overall and
	// the outcome calibration. Clamp scores and sum over the rubric keys (not
	// len(v.Dimensions)) so extra/unknown keys can't distort the mean.
	var sum float64
	for _, d := range Rubric {
		ds, ok := v.Dimensions[d.Key]
		if !ok {
			return fmt.Errorf("verdict missing dimension %q", d.Key)
		}
		ds.Score = clampScore(ds.Score)
		v.Dimensions[d.Key] = ds
		sum += float64(ds.Score)
	}
	// Recompute Overall only when out of range, or when it is the zero default
	// while the mean is non-zero (i.e. the model omitted it). A valid in-range
	// Overall is preserved even when it deviates from the mean — the prompt lets a
	// blocker dimension pull Overall below the average, and a genuine 0 (every
	// dimension scored 0) must survive too.
	mean := sum / float64(len(Rubric))
	if v.Overall < 0 || v.Overall > 10 || (v.Overall == 0 && mean != 0) {
		v.Overall = mean
	}
	return nil
}

// AgreesWithOutcome reports whether a verdict is consistent with the task's
// recorded landing outcome — a merged change should score at least threshold; a
// closed (abandoned) one should not. Used to validate the judge against ground
// truth before trusting it on unreviewed runs. Outcomes other than merged/closed
// are treated as agreement (no signal). Coarse until richer outcome labels
// (merge-with-edits, revert) land in #1082.
func AgreesWithOutcome(v QualityVerdict, outcome string, threshold float64) bool {
	switch outcome {
	case "merged":
		return v.Overall >= threshold
	case "closed":
		return v.Overall < threshold
	default:
		return true
	}
}

// judgeExtractLastJSON returns the last balanced {...} substring in s, or "".
// The model may prepend prose before the final JSON object.
func judgeExtractLastJSON(s string) string {
	s = strings.TrimSpace(s)
	var (
		inString  bool
		escape    bool
		depth     int
		objStart  = -1
		lastStart = -1
		lastEnd   = -1
	)
	for i := range len(s) {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				objStart = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && objStart >= 0 {
				lastStart = objStart
				lastEnd = i
				objStart = -1
			}
		}
	}
	if lastStart < 0 {
		return ""
	}
	return s[lastStart : lastEnd+1]
}

func clampScore(s int) int {
	if s < 0 {
		return 0
	}
	if s > 10 {
		return 10
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary so the cut never splits a multibyte character.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n…(truncated)"
}
