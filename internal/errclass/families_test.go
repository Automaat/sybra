package errclass

import (
	"strings"
	"testing"
)

func allFamilies() map[string][]string {
	return map[string][]string{
		"NetworkPhrases":      NetworkPhrases,
		"DNSPhrases":          DNSPhrases,
		"TLSPhrases":          TLSPhrases,
		"GatewayPhrases":      GatewayPhrases,
		"RateLimitPhrases":    RateLimitPhrases,
		"AuthPhrases":         AuthPhrases,
		"StreamPhrases":       StreamPhrases,
		"GitTransportPhrases": GitTransportPhrases,
		"BadRefPhrases":       BadRefPhrases,
	}
}

// TestNoPhraseSubsumesAnother catches the dead weight a substring-matched
// table accumulates: an entry contained in another entry can never be the
// reason a match happened, so a test naming it proves nothing.
func TestNoPhraseSubsumesAnother(t *testing.T) {
	type entry struct{ family, phrase string }
	var all []entry
	for family, phrases := range allFamilies() {
		for _, p := range phrases {
			all = append(all, entry{family, p})
		}
	}
	for _, a := range all {
		for _, b := range all {
			if a == b || a.phrase == b.phrase {
				continue
			}
			if strings.Contains(a.phrase, b.phrase) {
				t.Errorf("%s %q contains %s %q, so the longer entry is unreachable", a.family, a.phrase, b.family, b.phrase)
			}
		}
	}
}

func TestFamiliesAreUsableLowercase(t *testing.T) {
	for name, family := range allFamilies() {
		if len(family) == 0 {
			t.Errorf("%s is empty", name)
		}
		for _, phrase := range family {
			if phrase == "" {
				t.Errorf("%s has an empty phrase, which matches everything", name)
			}
			if phrase != strings.ToLower(phrase) {
				t.Errorf("%s has %q, which is not lowercase and can never match", name, phrase)
			}
			if strings.TrimSpace(phrase) != phrase {
				t.Errorf("%s has %q with surrounding whitespace", name, phrase)
			}
		}
	}
}

// TestEveryPhraseIsReachable proves each entry is load-bearing: the phrase on
// its own classifies, so deleting it would change an answer.
func TestEveryPhraseIsReachable(t *testing.T) {
	for name, family := range allFamilies() {
		for _, phrase := range family {
			if !Matches(phrase, family) {
				t.Errorf("%s %q does not match itself", name, phrase)
			}
		}
	}
}

// TestPermanentTLSFailureIsNotTransient pins the defect that made an x509
// failure retry forever without ever reaching a human.
func TestPermanentTLSFailureIsNotTransient(t *testing.T) {
	const x509 = `Get "https://api.github.com/repos/o/r": tls: failed to verify certificate: x509: certificate signed by unknown authority`
	if IsNetwork(x509) {
		t.Fatal("IsNetwork matched an x509 verification failure")
	}
	if IsGitTransport(x509) {
		t.Fatal("IsGitTransport matched an x509 verification failure")
	}
	if got := Classify(x509); got != Unknown {
		t.Fatalf("Classify(x509) = %q, want %q", got, Unknown)
	}
	if !IsNetwork("net/http: TLS handshake timeout") {
		t.Fatal("a real handshake timeout should still be transient")
	}
}

// TestBlockedMergeIsNotTransient pins the defect that cut a blocked merge's
// backoff ceiling from two hours to ten minutes.
func TestBlockedMergeIsNotTransient(t *testing.T) {
	for _, text := range []string{
		`Pull request is not mergeable: required status check "e2e" timed out`,
		"base branch policy prohibits the merge; the required status check timed out",
	} {
		if IsNetwork(text) {
			t.Errorf("IsNetwork(%q) = true, want false", text)
		}
		if got := Classify(text); got != Unknown {
			t.Errorf("Classify(%q) = %q, want %q", text, got, Unknown)
		}
	}
}

// TestScopeHintIsNotAuth pins the defect that would have held the shared auth
// circuit open on a missing-scope error.
func TestScopeHintIsNotAuth(t *testing.T) {
	const scopeHint = "gh: Your token has not been granted the required scopes. To request it, run: gh auth refresh -s read:org"
	if IsAuth(scopeHint) {
		t.Fatal("IsAuth matched a missing-scope hint, which never self-heals")
	}
}
