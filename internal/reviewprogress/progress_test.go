package reviewprogress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProgressIsBoundedAndNotAVerdict(t *testing.T) {
	packet := Start + `{"inspected":["storage transaction"],"findings":["verify lock order"],"remaining":["concurrent retry"]}` + End
	p, err := Parse(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Inspected) != 1 || !strings.Contains(Prompt(&p), "concurrent retry") {
		t.Fatal("progress was not resumed")
	}
	for _, bad := range []string{
		Start + `{"verdict":"CLEAN"}` + End,
		Start + `{"inspected":[],"findings":[],"remaining":[],"verdict":"CLEAN"}` + End,
		Start + `{"inspected":[],"findings":[]}` + End,
		Start + `{"inspected":[],"findings":[],"remaining":[]} {}` + End,
		Start + strings.Repeat("x", MaxBytes+1) + End,
		Start + `{"inspected":["` + strings.Repeat("x", MaxItemBytes+1) + `"],"findings":[],"remaining":[]}` + End,
		"prefix " + packet, packet + " trailing",
		Start + `{"inspected":["` + Start + `"],"findings":[],"remaining":[]}` + End,
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("invalid checkpoint accepted %.80q", bad)
		}
	}
	p.Inspected = make([]string, MaxItems+1)
	for i := range p.Inspected {
		p.Inspected[i] = "checked"
	}
	if err := p.Validate(); err == nil {
		t.Fatal("unbounded item count")
	}
}

func TestCheckpointDoesNotRecursivelySeedPrompts(t *testing.T) {
	p := Progress{Inspected: []string{"checked"}, Findings: []string{}, Remaining: []string{"retry"}}
	for range 10 {
		seed := Prompt(&p)
		if strings.Count(seed, Start) != 1 || len(seed) > MaxBytes+2048 {
			t.Fatal("recursive or unbounded seed")
		}
		data, _ := json.Marshal(p)
		var err error
		p, err = Parse(Start + string(data) + End)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !IsCheckpoint(Start+"broken") || !IsCheckpoint("broken"+End) {
		t.Fatal("malformed progress could enter final-review salvage")
	}
}
