package github

import "testing"

func TestFetchReviewsWith_KeepsSelfAuthoredBotPRDropsThirdParty(t *testing.T) {
	t.Parallel()
	createdResponse := `{
		"data": {
			"viewer": {"login": "sybra-app[bot]"},
			"search": {
				"nodes": [
					{
						"number": 4242,
						"title": "my own PR with failing CI",
						"url": "https://github.com/o/r/pull/4242",
						"headRefName": "feat/x",
						"author": {"login": "sybra-app", "type": "Bot"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": [{
							"commit": {
								"oid": "sha-fail",
								"statusCheckRollup": {
									"state": "FAILURE",
									"contexts": {"nodes": [
										{"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "FAILURE"}
									]}
								}
							}
						}]},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": []}
					},
					{
						"number": 9001,
						"title": "renovate PR authored by a third-party bot",
						"url": "https://github.com/o/r/pull/9001",
						"headRefName": "renovate/dep",
						"author": {"login": "renovate[bot]", "type": "Bot"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": [{"commit": {"oid": "sha-reno", "statusCheckRollup": {"state": "SUCCESS", "contexts": {"nodes": []}}}}]},
						"reviewThreads": {"nodes": []},
						"latestReviews": {"nodes": []}
					}
				]
			}
		}
	}`
	emptyResponse := `{
		"data": {
			"viewer": {"login": "sybra-app[bot]"},
			"search": {"nodes": []}
		}
	}`

	fe := &sequenceExecer{outputs: [][]byte{
		[]byte(createdResponse),
		[]byte(emptyResponse),
		[]byte(emptyResponse),
	}}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(summary.CreatedByMe) != 1 {
		t.Fatalf("CreatedByMe len = %d, want 1 (self-authored app-bot PR kept, third-party renovate bot dropped)", len(summary.CreatedByMe))
	}
	pr := summary.CreatedByMe[0]
	if pr.Number != 4242 {
		t.Errorf("kept PR number = %d, want 4242 (the self-authored one)", pr.Number)
	}
	if pr.CIStatus != "FAILURE" {
		t.Errorf("CIStatus = %q, want FAILURE", pr.CIStatus)
	}
}
