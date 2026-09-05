package intervention

import "testing"

func TestFingerprint_StableAcrossEquivalentInputs(t *testing.T) {
	a := Record{
		BlockerKind:         "operator_decision",
		BlockerCode:         "no_project_assigned",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
		// Fields that vary per-occurrence and must not affect the fingerprint.
		TaskID:         "task-a",
		OperatorReason: "assigned project after confirming with the team",
	}
	b := Record{
		BlockerKind:         "operator_decision",
		BlockerCode:         "  No_Project_Assigned  ", // differs only in case/whitespace
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
		TaskID:              "task-b",
		OperatorReason:      "totally different reason",
	}
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatalf("Fingerprint(a)=%q, Fingerprint(b)=%q, want equal for equivalent interventions", Fingerprint(a), Fingerprint(b))
	}
}

func TestFingerprint_DistinctForDistinctInterventions(t *testing.T) {
	base := Record{
		BlockerKind:         "operator_decision",
		BlockerCode:         "no_project_assigned",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
	}
	variants := []Record{
		base,
		withBlockerKind(base, "credential_required"),
		withBlockerCode(base, "task_cost_exceeded"),
		withActionClass(base, OperatorActionAutoRecovery),
		withToStatus(base, "ready-pr"),
	}
	seen := map[string]bool{}
	for i, rec := range variants {
		fp := Fingerprint(rec)
		if seen[fp] {
			t.Fatalf("variant %d fingerprint %q collided with an earlier distinct variant", i, fp)
		}
		seen[fp] = true
	}
}

func withBlockerKind(rec Record, kind string) Record {
	rec.BlockerKind = kind
	return rec
}

func withBlockerCode(rec Record, code string) Record {
	rec.BlockerCode = code
	return rec
}

func withActionClass(rec Record, class OperatorActionClass) Record {
	rec.OperatorActionClass = class
	return rec
}

func withToStatus(rec Record, status string) Record {
	rec.ToStatus = status
	return rec
}
