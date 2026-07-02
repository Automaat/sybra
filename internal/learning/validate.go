package learning

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// maxBucketItems and maxItemChars bound every free-text bucket so a digest
// stays a narrative retrospective, never a raw dump. maxTakeaways bounds the
// combined prompt/skill/model takeaway count.
const (
	maxBucketItems = 20
	maxItemChars   = 400
	maxTakeaways   = 20
)

// lowSampleMarkers are substrings (case-insensitive) that count as an
// explicit low-sample caveat in a takeaway's text.
var lowSampleMarkers = []string{
	"low sample", "insufficient sample", "insufficient data", "not enough data",
	"limited sample", "low confidence", "small sample", "n=",
}

// rawDigest is the summarizer's JSON output shape before server-assigned
// identity fields (SchemaVersion, GeneratedAt, ReportDigest, AuthorProvider,
// AuthorModel) are attached in Service.RunNow.
type rawDigest struct {
	Since           string     `json:"since"`
	Until           string     `json:"until"`
	Worked          []string   `json:"worked"`
	NotWorked       []string   `json:"notWorked"`
	Uncertain       []string   `json:"uncertain"`
	NextBets        []string   `json:"nextBets"`
	PromptTakeaways []Takeaway `json:"promptTakeaways"`
	SkillTakeaways  []Takeaway `json:"skillTakeaways"`
	ModelTakeaways  []Takeaway `json:"modelTakeaways"`
}

// parseDigestJSON extracts the last balanced JSON object from the
// summarizer's free-form output and unmarshals it into a rawDigest.
func parseDigestJSON(text string) (rawDigest, error) {
	jsonStr := extractLastJSON(text)
	if jsonStr == "" {
		return rawDigest{}, fmt.Errorf("no JSON object in summarizer output: %q", truncateForError(text))
	}
	var rd rawDigest
	if err := json.Unmarshal([]byte(jsonStr), &rd); err != nil {
		return rawDigest{}, fmt.Errorf("unmarshal digest JSON: %w", err)
	}
	return rd, nil
}

// validateDigest enforces the strict output contract: the echoed window must
// match the packet's actual window, at least one of worked/notWorked/
// uncertain plus nextBets must be present, every bucket and takeaway is
// bounded, and — when the packet carried experiment rows — any takeaway
// referencing one must carry its variant id and, if that variant is
// low-sample, an explicit caveat.
func validateDigest(rd rawDigest, pkt Packet) (Digest, error) {
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(rd.Since))
	if err != nil {
		return Digest{}, fmt.Errorf("invalid since %q: %w", rd.Since, err)
	}
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(rd.Until))
	if err != nil {
		return Digest{}, fmt.Errorf("invalid until %q: %w", rd.Until, err)
	}
	if !since.Equal(pkt.Since) || !until.Equal(pkt.Until) {
		return Digest{}, fmt.Errorf("window mismatch: got [%s,%s), want [%s,%s)",
			since.Format(time.RFC3339), until.Format(time.RFC3339),
			pkt.Since.Format(time.RFC3339), pkt.Until.Format(time.RFC3339))
	}

	if len(rd.Worked) == 0 && len(rd.NotWorked) == 0 && len(rd.Uncertain) == 0 {
		return Digest{}, fmt.Errorf("digest has no worked, notWorked, or uncertain findings")
	}
	if len(rd.NextBets) == 0 {
		return Digest{}, fmt.Errorf("digest missing nextBets")
	}
	for _, bucket := range []struct {
		name  string
		items []string
	}{
		{"worked", rd.Worked},
		{"notWorked", rd.NotWorked},
		{"uncertain", rd.Uncertain},
		{"nextBets", rd.NextBets},
	} {
		if err := boundBucket(bucket.name, bucket.items); err != nil {
			return Digest{}, err
		}
	}

	allTakeaways := make([]Takeaway, 0, len(rd.PromptTakeaways)+len(rd.SkillTakeaways)+len(rd.ModelTakeaways))
	allTakeaways = append(allTakeaways, rd.PromptTakeaways...)
	allTakeaways = append(allTakeaways, rd.SkillTakeaways...)
	allTakeaways = append(allTakeaways, rd.ModelTakeaways...)
	if len(allTakeaways) > maxTakeaways {
		return Digest{}, fmt.Errorf("too many takeaways: %d (max %d)", len(allTakeaways), maxTakeaways)
	}

	insufficientByRef := make(map[string]bool, len(pkt.Experiments))
	for i := range pkt.Experiments {
		insufficientByRef[pkt.Experiments[i].ExperimentID+"|"+pkt.Experiments[i].VariantID] = pkt.Experiments[i].InsufficientData
	}
	for _, tw := range allTakeaways {
		if len(tw.Text) > maxItemChars {
			return Digest{}, fmt.Errorf("takeaway text exceeds %d characters", maxItemChars)
		}
		if tw.ExperimentRef == "" {
			continue
		}
		if tw.VariantRef == "" {
			return Digest{}, fmt.Errorf("takeaway references experiment %q without a variantRef", tw.ExperimentRef)
		}
		if insufficient, ok := insufficientByRef[tw.ExperimentRef+"|"+tw.VariantRef]; ok && insufficient && !mentionsLowSample(tw.Text) {
			return Digest{}, fmt.Errorf("takeaway for low-sample variant %s/%s must state the sample caveat", tw.ExperimentRef, tw.VariantRef)
		}
	}

	return Digest{
		Since:           since,
		Until:           until,
		Worked:          rd.Worked,
		NotWorked:       rd.NotWorked,
		Uncertain:       rd.Uncertain,
		NextBets:        rd.NextBets,
		PromptTakeaways: rd.PromptTakeaways,
		SkillTakeaways:  rd.SkillTakeaways,
		ModelTakeaways:  rd.ModelTakeaways,
	}, nil
}

func boundBucket(name string, items []string) error {
	if len(items) > maxBucketItems {
		return fmt.Errorf("%s has %d items (max %d)", name, len(items), maxBucketItems)
	}
	for _, s := range items {
		if len(s) > maxItemChars {
			return fmt.Errorf("%s item exceeds %d characters", name, maxItemChars)
		}
	}
	return nil
}

func mentionsLowSample(text string) bool {
	lower := strings.ToLower(text)
	for _, m := range lowSampleMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func truncateForError(s string) string {
	const maxChars = 200
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…"
}

// extractLastJSON returns the last balanced {...} substring in s, or "".
// Mirrors evaluation.judgeExtractLastJSON / selfmonitor.judgeExtractLastJSON —
// each LLM-output-parsing package keeps its own copy rather than sharing a
// dependency across otherwise-independent judge/summarizer packages.
func extractLastJSON(s string) string {
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
