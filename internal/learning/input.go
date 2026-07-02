package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/stats"
)

// ExperimentSignal is one A/B experiment/variant row surfaced to the
// summarizer: aggregate counts and sample-readiness only, never raw run
// content. Mirrors the fields of evaluation.ComparisonBreakdown the
// summarizer actually needs to cite a variant and caveat a low-sample one.
type ExperimentSignal struct {
	ExperimentID         string  `json:"experimentId"`
	Kind                 string  `json:"kind"`
	VariantID            string  `json:"variantId"`
	Provider             string  `json:"provider,omitempty"`
	Model                string  `json:"model,omitempty"`
	Runs                 int     `json:"runs"`
	MergeRate            float64 `json:"mergeRate"`
	FailureRate          float64 `json:"failureRate"`
	InsufficientData     bool    `json:"insufficientData"`
	SampleStatus         string  `json:"sampleStatus,omitempty"`
	MinSamplesPerVariant int     `json:"minSamplesPerVariant,omitempty"`
}

// Packet is the curated, aggregate-only input assembled for the digest
// summarizer. It never carries raw logs, task bodies, URLs, branches, or
// SHAs — only counts, rates, and identifiers that are already safe to
// narrate. ReportDigest is a content hash of the aggregate data (not model
// output), so a repeated tick over an unchanged window/report produces the
// same Digest.Key() and dedups in Store.Put.
type Packet struct {
	Since, Until     time.Time
	ReportDigest     string
	Overall          evaluation.Scorecard
	ByProvider       []evaluation.Breakdown
	ByRole           []evaluation.Breakdown
	Experiments      []ExperimentSignal
	PreviousNextBets []string
}

// windowFor derives the [since, until) span for the next digest: contiguous
// from the previous digest's Until when one exists, or now-windowDays on a
// cold start (no prior digest). The span is clamped so it never reaches more
// than maxWindowDays behind now — a fresh install or an instance resumed
// after a long idle period must never feed the summarizer an unbounded
// window. Both bounds are truncated to whole seconds: buildPrompt echoes
// them with second precision (time.RFC3339), and a real summarizer can only
// echo back what it was shown — comparing against a sub-second-precision
// pkt.Since/Until would fail validateDigest's window-match check on every
// single well-behaved response.
func windowFor(now time.Time, prev *Digest, windowDays, maxWindowDays int) (since, until time.Time) {
	until = now.Truncate(time.Second)
	if prev != nil && !prev.Until.IsZero() {
		since = prev.Until // cold start: no previous digest, fall back to now-windowDays
	} else {
		since = now.AddDate(0, 0, -windowDays)
	}
	since = since.Truncate(time.Second)
	if floor := now.AddDate(0, 0, -maxWindowDays).Truncate(time.Second); since.Before(floor) {
		since = floor
	}
	if !since.Before(until) {
		since = until.AddDate(0, 0, -1)
	}
	return since, until
}

// freshSignal counts stats run records and task.landed audit events falling
// inside [since, until) — the two thresholds Service.RunNow gates generation
// on, so a mostly-idle fleet does not produce an empty or noisy digest.
func freshSignal(recs []stats.RunRecord, evts []audit.Event, since, until time.Time) (freshRuns, landed int) {
	inWindow := func(t time.Time) bool { return !t.Before(since) && t.Before(until) }
	for i := range recs {
		if inWindow(recs[i].Timestamp) {
			freshRuns++
		}
	}
	for i := range evts {
		if evts[i].Type == audit.EventTaskLanded && inWindow(evts[i].Timestamp) {
			landed++
		}
	}
	return freshRuns, landed
}

// buildPacket assembles the curated input packet for [since, until) from
// stats run records, audit events, and the configured A/B experiments.
// Anchors "worked" on real landings via evaluation.Compute/BreakdownBy/
// CompareVariants — the same pure aggregation evaluation.Service uses —
// just bound to the digest's own contiguous window instead of evaluation's
// independent rolling window.
func buildPacket(recs []stats.RunRecord, evts []audit.Event, abTesting abtest.Config, since, until time.Time, prev *Digest) Packet {
	pkt := Packet{
		Since:   since,
		Until:   until,
		Overall: evaluation.Compute(recs, evts, since, until),
		ByProvider: evaluation.BreakdownBy(recs, since, until, func(r stats.RunRecord) string {
			return r.Provider
		}),
		ByRole: evaluation.BreakdownBy(recs, since, until, func(r stats.RunRecord) string {
			return r.Role
		}),
	}

	kindByExperiment := make(map[string]string, len(abTesting.Experiments))
	for i := range abTesting.Experiments {
		kindByExperiment[abTesting.Experiments[i].ID] = abTesting.Experiments[i].KindValue()
	}
	rows := evaluation.CompareVariants(recs, evts, since, until, abTesting.MinSamplesPerVariant)
	pkt.Experiments = make([]ExperimentSignal, 0, len(rows))
	for i := range rows {
		kind := kindByExperiment[rows[i].ExperimentID]
		if kind == "" {
			kind = "unknown"
		}
		pkt.Experiments = append(pkt.Experiments, ExperimentSignal{
			ExperimentID:         rows[i].ExperimentID,
			Kind:                 kind,
			VariantID:            rows[i].VariantID,
			Provider:             rows[i].Provider,
			Model:                rows[i].Model,
			Runs:                 rows[i].Runs,
			MergeRate:            rows[i].MergeRate,
			FailureRate:          rows[i].FailureRate,
			InsufficientData:     rows[i].InsufficientData,
			SampleStatus:         rows[i].SampleStatus,
			MinSamplesPerVariant: rows[i].MinSamplesPerVariant,
		})
	}

	if prev != nil {
		pkt.PreviousNextBets = prev.NextBets
	}

	pkt.ReportDigest = pkt.hash()
	return pkt
}

// hash returns a deterministic content hash of the packet's aggregate data
// (never the model's output) so repeated ticks over an unchanged
// window/report reuse the same Digest.Key() and dedup in Store.Put.
func (p Packet) hash() string {
	data, err := json.Marshal(struct {
		Overall          evaluation.Scorecard   `json:"overall"`
		ByProvider       []evaluation.Breakdown `json:"byProvider"`
		ByRole           []evaluation.Breakdown `json:"byRole"`
		Experiments      []ExperimentSignal     `json:"experiments"`
		PreviousNextBets []string               `json:"previousNextBets"`
	}{p.Overall, p.ByProvider, p.ByRole, p.Experiments, p.PreviousNextBets})
	if err != nil {
		// json.Marshal only fails on unsupported types (channels, funcs) —
		// none of which appear in this struct — so this is unreachable in
		// practice. Fall back to a stable-but-degenerate hash rather than
		// panicking, since ReportDigest is dedup identity, not correctness.
		data = []byte(fmt.Sprintf("%v", p))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// buildPrompt renders the packet into the summarizer's instruction prompt.
// Every fact the model can cite is already aggregate (counts, rates, ids);
// the prompt forbids inventing anything not present in the packet.
func buildPrompt(pkt Packet) string {
	var b strings.Builder

	b.WriteString("You are writing a periodic engineering retrospective (\"Learning Digest\") ")
	b.WriteString("for an autonomous coding-agent fleet called Sybra, from aggregate metrics only.\n")
	b.WriteString("Use ONLY the data below — never invent task names, PR numbers, URLs, branches, or commit SHAs.\n")
	b.WriteString("Output ONLY a single JSON object on the final line, matching the schema exactly.\n\n")

	sinceStr := pkt.Since.UTC().Format(time.RFC3339)
	untilStr := pkt.Until.UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "Window: since=%s until=%s\n\n", sinceStr, untilStr)

	if overall, err := json.Marshal(pkt.Overall); err == nil {
		fmt.Fprintf(&b, "Overall scorecard:\n%s\n\n", overall)
	}
	if len(pkt.ByProvider) > 0 {
		if by, err := json.Marshal(pkt.ByProvider); err == nil {
			fmt.Fprintf(&b, "By provider:\n%s\n\n", by)
		}
	}
	if len(pkt.ByRole) > 0 {
		if by, err := json.Marshal(pkt.ByRole); err == nil {
			fmt.Fprintf(&b, "By role:\n%s\n\n", by)
		}
	}
	if len(pkt.Experiments) > 0 {
		if exp, err := json.Marshal(pkt.Experiments); err == nil {
			b.WriteString("Active A/B experiments (variant-level; insufficientData=true means the sample is below ")
			b.WriteString("the configured minimum — treat conclusions from these rows as low-confidence and say so):\n")
			fmt.Fprintf(&b, "%s\n\n", exp)
		}
	}
	if len(pkt.PreviousNextBets) > 0 {
		if prev, err := json.Marshal(pkt.PreviousNextBets); err == nil {
			b.WriteString("Bets proposed in the previous digest (report on their disposition where the data above supports it):\n")
			fmt.Fprintf(&b, "%s\n\n", prev)
		}
	}

	fmt.Fprintf(&b, `Output schema (single JSON object, nothing else):
{
  "since": %q,
  "until": %q,
  "worked": ["one plain sentence per finding"],
  "notWorked": ["one plain sentence per finding"],
  "uncertain": ["one plain sentence per low-sample or ambiguous finding"],
  "nextBets": ["one plain sentence per proposed next experiment"],
  "promptTakeaways": [{"text":"...","experimentRef":"optional experiment id","variantRef":"optional variant id"}],
  "skillTakeaways": [{"text":"...","experimentRef":"...","variantRef":"..."}],
  "modelTakeaways": [{"text":"...","experimentRef":"...","variantRef":"..."}]
}

Rules:
- Echo since/until exactly as given above — do not reformat or shift them.
- worked/notWorked/uncertain/nextBets: each at most %d items, each item at most %d characters.
- uncertain is for findings with too little data to be confident.
- Any prompt/skill/model takeaway that references one of the experiments above MUST set variantRef to that
  variant's exact id, and if that variant's insufficientData is true, the takeaway text MUST state the sample
  caveat explicitly (e.g. "low sample, N=12").
- Do not fabricate specifics not present in the data above.
`, sinceStr, untilStr, maxBucketItems, maxItemChars)

	return b.String()
}
