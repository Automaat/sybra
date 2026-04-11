package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renovatePRQuery includes individual check run contexts for rerun support.
const renovatePRQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        headRefName
        isDraft
        mergeable
        createdAt
        updatedAt
        reviewDecision
        author { login type: __typename }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    ... on CheckRun {
                      name
                      status
                      conclusion
                    }
                  }
                }
              }
            }
          }
        }
        reviewThreads(first: 100) {
          nodes { isResolved }
        }
        latestReviews(first: 20) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

// FetchRenovatePRs returns Renovate bot PRs for the given repositories.
func FetchRenovatePRs(author string, repos []string) ([]RenovatePR, error) {
	return fetchRenovatePRsWith(defaultExecer, author, repos)
}

func fetchRenovatePRsWith(e execer, author string, repos []string) ([]RenovatePR, error) {
	if len(repos) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var all []RenovatePR

	authors := []string{author}
	if author == "app/renovate" {
		authors = append(authors, "renovate[bot]")
	}

	for _, repo := range repos {
		for _, a := range authors {
			query := fmt.Sprintf("is:pr is:open author:%s repo:%s", a, repo)
			prs, err := searchRenovatePRsWith(e, query)
			if err != nil {
				return nil, fmt.Errorf("fetch renovate PRs for %s: %w", repo, err)
			}
			for i := range prs {
				key := fmt.Sprintf("%s#%d", prs[i].Repository, prs[i].Number)
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					all = append(all, prs[i])
				}
			}
		}
	}
	return all, nil
}

func searchRenovatePRsWith(e execer, query string) ([]RenovatePR, error) {
	out, err := e.run("api", "graphql",
		"-f", "query="+renovatePRQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp gqlResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	return convertRenovatePRs(resp.Data.Search.Nodes, viewerLogin(e)), nil
}

func convertRenovatePRs(nodes []gqlPR, viewer string) []RenovatePR {
	prs := make([]RenovatePR, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		pr := convertCommonPR(n, viewer)

		var checks []CheckRunInfo
		if len(n.Commits.Nodes) > 0 {
			if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
				for _, ctx := range rollup.Contexts.Nodes {
					if ctx.Name == "" {
						continue
					}
					checks = append(checks, CheckRunInfo(ctx))
				}
			}
		}

		prs = append(prs, RenovatePR{PullRequest: pr, CheckRuns: checks})
	}
	return prs
}
