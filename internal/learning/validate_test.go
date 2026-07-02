package learning

import (
	"strings"
	"testing"
	"time"
)

func testWindow() (since, until time.Time) {
	since = time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	until = time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	return since, until
}

func validRawJSON(since, until time.Time) string {
	return `{
  "since": "` + since.Format(time.RFC3339) + `",
  "until": "` + until.Format(time.RFC3339) + `",
  "worked": ["claude sonnet landed cleanly on implementation tasks"],
  "notWorked": ["copilot review left threads unresolved"],
  "uncertain": [],
  "nextBets": ["try a longer prompt for skill X"]
}`
}

func TestParseDigestJSONExtractsLastObjectFromProse(t *testing.T) {
	since, until := testWindow()
	text := "Here is my analysis.\n\n" + validRawJSON(since, until) + "\ntrailing text should be ignored"
	rd, err := parseDigestJSON(text)
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	if len(rd.Worked) != 1 {
		t.Fatalf("Worked = %+v, want 1 item", rd.Worked)
	}
}

func TestParseDigestJSONMalformedReturnsError(t *testing.T) {
	for name, text := range map[string]string{
		"no json at all": "the model refused to answer",
		"truncated json": `{"worked": ["a"` + "",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDigestJSON(text); err == nil {
				t.Fatalf("parseDigestJSON(%q) returned no error, want one", text)
			}
		})
	}
}

func TestValidateDigestAccepts(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	pkt := Packet{Since: since, Until: until}

	d, err := validateDigest(rd, pkt)
	if err != nil {
		t.Fatalf("validateDigest returned error: %v", err)
	}
	if !d.Since.Equal(since) || !d.Until.Equal(until) {
		t.Fatalf("digest window = [%s,%s), want [%s,%s)", d.Since, d.Until, since, until)
	}
	if len(d.Worked) != 1 || len(d.NextBets) != 1 {
		t.Fatalf("digest = %+v, want worked+nextBets carried through", d)
	}
}

func TestValidateDigestRejectsWindowMismatch(t *testing.T) {
	since, until := testWindow()
	wrongUntil := until.AddDate(0, 0, 1)
	rd, err := parseDigestJSON(validRawJSON(since, wrongUntil))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	pkt := Packet{Since: since, Until: until}

	if _, err := validateDigest(rd, pkt); err == nil {
		t.Fatal("validateDigest accepted a window that does not match the packet's actual window")
	}
}

func TestValidateDigestRejectsMissingBuckets(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(`{
  "since": "` + since.Format(time.RFC3339) + `",
  "until": "` + until.Format(time.RFC3339) + `",
  "worked": [],
  "notWorked": [],
  "uncertain": [],
  "nextBets": []
}`)
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	pkt := Packet{Since: since, Until: until}
	if _, err := validateDigest(rd, pkt); err == nil {
		t.Fatal("validateDigest accepted a digest with every bucket empty")
	}
}

func TestValidateDigestRejectsOverLengthItem(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	rd.Worked = []string{strings.Repeat("x", maxItemChars+1)}
	pkt := Packet{Since: since, Until: until}

	if _, err := validateDigest(rd, pkt); err == nil {
		t.Fatal("validateDigest accepted an over-length bucket item")
	}
}

func TestValidateDigestRejectsOverCountBucket(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	items := make([]string, maxBucketItems+1)
	for i := range items {
		items[i] = "finding"
	}
	rd.Worked = items
	pkt := Packet{Since: since, Until: until}

	if _, err := validateDigest(rd, pkt); err == nil {
		t.Fatal("validateDigest accepted a bucket over maxBucketItems")
	}
}

func TestValidateDigestRequiresVariantRefForExperimentTakeaway(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	rd.ModelTakeaways = []Takeaway{{Text: "claude wins on this experiment", ExperimentRef: "exp-1"}}
	pkt := Packet{
		Since: since, Until: until,
		Experiments: []ExperimentSignal{{ExperimentID: "exp-1", VariantID: "a", InsufficientData: false}},
	}

	if _, err := validateDigest(rd, pkt); err == nil {
		t.Fatal("validateDigest accepted a takeaway with experimentRef but no variantRef")
	}
}

func TestValidateDigestRequiresLowSampleCaveat(t *testing.T) {
	since, until := testWindow()
	rd, err := parseDigestJSON(validRawJSON(since, until))
	if err != nil {
		t.Fatalf("parseDigestJSON returned error: %v", err)
	}
	pkt := Packet{
		Since: since, Until: until,
		Experiments: []ExperimentSignal{{ExperimentID: "exp-1", VariantID: "a", InsufficientData: true}},
	}

	t.Run("missing caveat rejected", func(t *testing.T) {
		rd := rd
		rd.ModelTakeaways = []Takeaway{{Text: "claude wins on this experiment", ExperimentRef: "exp-1", VariantRef: "a"}}
		if _, err := validateDigest(rd, pkt); err == nil {
			t.Fatal("validateDigest accepted a low-sample takeaway without a caveat")
		}
	})

	t.Run("explicit caveat accepted", func(t *testing.T) {
		rd := rd
		rd.ModelTakeaways = []Takeaway{{Text: "claude leads, but low sample (N=4)", ExperimentRef: "exp-1", VariantRef: "a"}}
		if _, err := validateDigest(rd, pkt); err != nil {
			t.Fatalf("validateDigest rejected a properly-caveated low-sample takeaway: %v", err)
		}
	})
}
