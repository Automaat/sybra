// Package learning defines the Learning Digest data model: a periodic,
// human-readable retrospective distilled from evaluation reports and run
// history.
//
// The store is local-debug-only and raw: digests are never scrubbed at write
// time. Any future code that surfaces a digest in a GitHub issue/PR/comment
// MUST first route through App.workScrubContextForTask + scrub.Scrub — the
// store deliberately does not scrub (see Digest.Scrub, which the surfacing
// caller must invoke explicitly).
package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/Automaat/sybra/internal/scrub"
)

// SchemaVersion is the current on-disk schema version for Digest. Bump when
// making a breaking change to the persisted shape.
const SchemaVersion = 1

// MaxEvidenceRefs bounds how many evidence references a single digest may
// carry — digests are narrative retrospectives, not raw log dumps.
const MaxEvidenceRefs = 20

// Takeaway is a single distilled lesson, optionally traceable back to the
// A/B experiment and variant it was derived from.
type Takeaway struct {
	Text          string `json:"text"`
	ExperimentRef string `json:"experimentRef,omitempty"`
	VariantRef    string `json:"variantRef,omitempty"`
}

// EvidenceRef points at supporting material (a task, an artifact, an audit
// event) without embedding the raw content — digests stay small and
// human-readable.
type EvidenceRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Key identifies a digest's source window and input report, and is the
// dedup identity: repeated refreshes over the same window/report must not
// produce duplicate digests.
type Key struct {
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	ReportDigest string    `json:"reportDigest"`
}

// Hash returns the deterministic on-disk filename stem for this key: a hex
// sha256 over the normalized (RFC3339Nano, UTC) Since and Until timestamps
// and the ReportDigest, so two Keys that describe the same window/report
// hash identically regardless of the timezone or sub-second formatting the
// caller happened to construct them with.
func (k Key) Hash() string {
	sum := sha256.Sum256([]byte(
		k.Since.UTC().Format(time.RFC3339Nano) + "|" +
			k.Until.UTC().Format(time.RFC3339Nano) + "|" +
			k.ReportDigest,
	))
	return hex.EncodeToString(sum[:])
}

// Digest is one periodic retrospective: what worked, what didn't, and what
// to try next, along with typed takeaways for prompt/skill/model experiments
// and bounded evidence pointers.
//
// Until is the exclusive upper bound of the source window: a digest covers
// [Since, Until).
type Digest struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Since         time.Time `json:"since"`
	Until         time.Time `json:"until"`
	ReportDigest  string    `json:"reportDigest"`

	ProjectType    string `json:"projectType,omitempty"`
	AuthorProvider string `json:"authorProvider,omitempty"`
	AuthorModel    string `json:"authorModel,omitempty"`

	Worked    []string `json:"worked,omitempty"`
	NotWorked []string `json:"notWorked,omitempty"`
	NextBets  []string `json:"nextBets,omitempty"`

	PromptTakeaways []Takeaway `json:"promptTakeaways,omitempty"`
	SkillTakeaways  []Takeaway `json:"skillTakeaways,omitempty"`
	ModelTakeaways  []Takeaway `json:"modelTakeaways,omitempty"`

	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

// Key derives this digest's dedup identity. This is the single derivation
// site — callers must never recompute the hash inputs independently, or a
// second derivation drifting from this one would silently duplicate or drop
// digests.
func (d *Digest) Key() Key {
	return Key{Since: d.Since, Until: d.Until, ReportDigest: d.ReportDigest}
}

// Scrub redacts work-repo identifiers from every free-text field using
// blocklist (see internal/scrub), and returns the total number of
// redactions performed. The store itself never calls this — a caller
// surfacing a digest outside the raw local store must invoke it first.
func (d *Digest) Scrub(blocklist []string) int {
	total := 0
	scrubField := func(s string) string {
		out, n := scrub.Scrub(s, blocklist)
		total += n
		return out
	}
	scrubSlice := func(ss []string) {
		for i, s := range ss {
			ss[i] = scrubField(s)
		}
	}
	scrubTakeaways := func(ts []Takeaway) {
		for i := range ts {
			ts[i].Text = scrubField(ts[i].Text)
			ts[i].ExperimentRef = scrubField(ts[i].ExperimentRef)
			ts[i].VariantRef = scrubField(ts[i].VariantRef)
		}
	}

	d.ProjectType = scrubField(d.ProjectType)
	d.AuthorProvider = scrubField(d.AuthorProvider)
	d.AuthorModel = scrubField(d.AuthorModel)
	d.ReportDigest = scrubField(d.ReportDigest)

	scrubSlice(d.Worked)
	scrubSlice(d.NotWorked)
	scrubSlice(d.NextBets)

	scrubTakeaways(d.PromptTakeaways)
	scrubTakeaways(d.SkillTakeaways)
	scrubTakeaways(d.ModelTakeaways)

	for i := range d.Evidence {
		d.Evidence[i].Kind = scrubField(d.Evidence[i].Kind)
		d.Evidence[i].ID = scrubField(d.Evidence[i].ID)
	}

	return total
}
