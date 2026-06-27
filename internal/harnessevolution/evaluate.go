package harnessevolution

func EvaluateProposal(p Proposal, c Cluster, corpus []CorpusCase) EvaluationResult {
	var result EvaluationResult
	for _, tc := range corpus {
		if !caseMatches(tc, c) {
			continue
		}
		result.CasesRun++
		result.MatchedCases = append(result.MatchedCases, tc.ID)
		if tc.WantKind != p.Kind {
			result.Failures = append(result.Failures, tc.ID+": wrong proposal kind")
		}
		if tc.RequiresHumanApproval && !p.RequiresHumanApproval {
			result.Failures = append(result.Failures, tc.ID+": missing human approval gate")
		}
	}
	switch {
	case len(result.Failures) > 0:
		result.Recommendation = RecommendationReject
	case p.RequiresHumanApproval:
		result.Recommendation = RecommendationNeedsHumanReview
	case result.CasesRun == 0:
		result.Recommendation = RecommendationNeedsHumanReview
	default:
		result.Recommendation = RecommendationRecommend
	}
	return result
}

func caseMatches(tc CorpusCase, c Cluster) bool {
	if tc.Category != "" && normalizeToken(tc.Category) != normalizeToken(c.Category) {
		return false
	}
	if tc.FailureKind != "" && normalizeToken(tc.FailureKind) != normalizeToken(c.FailureKind) {
		return false
	}
	return true
}
