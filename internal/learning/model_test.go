package learning

import (
	"strings"
	"testing"
	"time"
)

func TestKeyHashDeterministicNormalized(t *testing.T) {
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)

	k1 := Key{Since: since, Until: until, ReportDigest: "abc123"}
	k2 := Key{Since: since.In(time.FixedZone("UTC+2", 2*60*60)), Until: until.In(time.FixedZone("UTC+2", 2*60*60)), ReportDigest: "abc123"}

	h1, h2 := k1.Hash(), k2.Hash()
	if h1 != h2 {
		t.Fatalf("hash should be timezone-invariant: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char hex sha256, got %d chars", len(h1))
	}

	k3 := Key{Since: since, Until: until, ReportDigest: "different"}
	if k3.Hash() == h1 {
		t.Fatal("different report digest must hash differently")
	}

	k4 := Key{Since: since.Add(time.Second), Until: until, ReportDigest: "abc123"}
	if k4.Hash() == h1 {
		t.Fatal("different since must hash differently")
	}
}

func TestScrubBoundaries(t *testing.T) {
	d := Digest{
		ProjectType:    "work",
		AuthorProvider: "claude",
		AuthorModel:    "sonnet",
		ReportDigest:   "digest-owner/repo-1",
		Worked:         []string{"shipped owner/repo #42"},
		NotWorked:      []string{"owner/repo flaked"},
		NextBets:       []string{"retry owner/repo"},
		PromptTakeaways: []Takeaway{
			{Text: "prompt referencing owner/repo", ExperimentRef: "owner/repo-exp", VariantRef: "owner/repo-v2"},
		},
		SkillTakeaways: []Takeaway{
			{Text: "skill mentions owner/repo"},
		},
		ModelTakeaways: []Takeaway{
			{Text: "model note about owner/repo"},
		},
		Evidence: []EvidenceRef{
			{Kind: "task", ID: "owner/repo-task-1"},
		},
	}

	blocklist := []string{"owner/repo"}
	total := d.Scrub(blocklist)

	if total == 0 {
		t.Fatal("expected at least one redaction")
	}

	allText := []string{
		d.ReportDigest,
		d.Worked[0], d.NotWorked[0], d.NextBets[0],
		d.PromptTakeaways[0].Text, d.PromptTakeaways[0].ExperimentRef, d.PromptTakeaways[0].VariantRef,
		d.SkillTakeaways[0].Text,
		d.ModelTakeaways[0].Text,
		d.Evidence[0].ID,
	}
	for _, s := range allText {
		if strings.Contains(s, "owner/repo") {
			t.Errorf("field still contains blocklisted term after Scrub: %q", s)
		}
	}
}
