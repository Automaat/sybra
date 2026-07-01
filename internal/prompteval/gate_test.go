package prompteval

import (
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

// defaultTestOfflineConfig returns the fail-closed default: UnavailablePolicy
// "fail" means a missing/unavailable verdict never allows enrollment.
func defaultTestOfflineConfig() config.OfflineEvalConfig {
	return config.OfflineEvalConfig{
		Runner:            "native",
		MinScore:          1.0,
		UnavailablePolicy: "fail",
	}
}

func TestGateBlocksOnFail(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	verdict := VariantVerdict{
		VariantID: "v1",
		Digest:    Digest([]byte("prompt-a")),
		Status:    StatusFail,
		Reason:    "assertion failed",
	}
	if err := store.Write(verdict); err != nil {
		t.Fatalf("Write: %v", err)
	}
	gate := NewGate(store, defaultTestOfflineConfig())
	allow, _, err := gate.AllowEnrollment(verdict.VariantID, verdict.Digest)
	if err != nil {
		t.Fatalf("AllowEnrollment: %v", err)
	}
	if allow {
		t.Fatal("AllowEnrollment allowed a FAIL verdict")
	}
}

func TestGateTriState(t *testing.T) {
	t.Parallel()

	t.Run("pass allows", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		v := VariantVerdict{VariantID: "v1", Digest: Digest([]byte("p")), Status: StatusPass}
		if err := store.Write(v); err != nil {
			t.Fatalf("Write: %v", err)
		}
		gate := NewGate(store, defaultTestOfflineConfig())
		allow, _, err := gate.AllowEnrollment(v.VariantID, v.Digest)
		if err != nil || !allow {
			t.Fatalf("allow=%v err=%v, want allow=true", allow, err)
		}
	})

	t.Run("unavailable fail-closed by default", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		v := VariantVerdict{VariantID: "v1", Digest: Digest([]byte("p")), Status: StatusUnavailable}
		if err := store.Write(v); err != nil {
			t.Fatalf("Write: %v", err)
		}
		gate := NewGate(store, defaultTestOfflineConfig())
		allow, _, err := gate.AllowEnrollment(v.VariantID, v.Digest)
		if err != nil || allow {
			t.Fatalf("allow=%v err=%v, want allow=false (fail-closed default)", allow, err)
		}
	})

	t.Run("unavailable allowed under explicit pass policy", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		v := VariantVerdict{VariantID: "v1", Digest: Digest([]byte("p")), Status: StatusUnavailable}
		if err := store.Write(v); err != nil {
			t.Fatalf("Write: %v", err)
		}
		cfg := defaultTestOfflineConfig()
		cfg.UnavailablePolicy = "pass"
		gate := NewGate(store, cfg)
		allow, _, err := gate.AllowEnrollment(v.VariantID, v.Digest)
		if err != nil || !allow {
			t.Fatalf("allow=%v err=%v, want allow=true under pass policy", allow, err)
		}
	})

	t.Run("no verdict recorded is fail-closed by default", func(t *testing.T) {
		t.Parallel()
		store := New(t.TempDir())
		gate := NewGate(store, defaultTestOfflineConfig())
		allow, _, err := gate.AllowEnrollment("missing-variant", Digest([]byte("p")))
		if err != nil || allow {
			t.Fatalf("allow=%v err=%v, want allow=false for a missing verdict", allow, err)
		}
	})
}
