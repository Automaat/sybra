package errclass

import (
	"strings"
	"testing"
)

func allFamilies() map[string][]string {
	return map[string][]string{
		"badRefPhrases":            badRefPhrases,
		"githubTransientPhrases":   githubTransientPhrases,
		"ghOutputTransientPhrases": ghOutputTransientPhrases,
		"githubRateLimitPhrases":   githubRateLimitPhrases,
		"githubAuthPhrases":        githubAuthPhrases,
		"gitTransportPhrases":      gitTransportPhrases,
		"workflowTransientPhrases": workflowTransientPhrases,
		"workflowAuthPhrases":      workflowAuthPhrases,
		"agentRateLimitPhrases":    agentRateLimitPhrases,
		"agentGitPhrases":          agentGitPhrases,
		"mergeBlockedPhrases":      mergeBlockedPhrases,
		"prFixPermanentPhrases":    prFixPermanentPhrases,
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
