package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const renovateSearchChunkSize = 20

// renovatePRQuery includes individual check run contexts for rerun support.
const renovatePRQuery = `query($q: String!) {
  viewer { login }
  search(query: $q, type: ISSUE, first: 100) {
    pageInfo {
      hasNextPage
      endCursor
    }
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
              oid
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    __typename
                    ... on CheckRun {
                      name
                      status
                      conclusion
                    }
                    ... on StatusContext {
                      name: context
                      state
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

	authors := []string{author}
	if author == "app/renovate" {
		authors = append(authors, "renovate[bot]")
	}
	cacheKey := strings.Join(authors, ",") + "||" + strings.Join(repos, ",")
	if runtimeCacheEnabled(e) {
		if cached, ok := renovatePRsCache.Get(cacheKey); ok {
			return cached, nil
		}
		if ghGate.shouldSkipOptional("graphql", priorityDiscovery) {
			if stale, ok := renovatePRsCache.GetStale(cacheKey); ok {
				return stale, nil
			}
			return nil, ErrBudgetExhausted
		}
	}

	seen := make(map[string]struct{})
	var all []RenovatePR

	for start := 0; start < len(repos); start += renovateSearchChunkSize {
		end := min(start+renovateSearchChunkSize, len(repos))
		query := buildRenovateSearchQuery(authors, repos[start:end])
		prs, err := searchRenovatePRsWith(e, query)
		if err != nil {
			if runtimeCacheEnabled(e) {
				if stale, ok := renovatePRsCache.GetStale(cacheKey); ok {
					return stale, nil
				}
			}
			return nil, fmt.Errorf("fetch renovate PRs: %w", err)
		}
		for i := range prs {
			key := fmt.Sprintf("%s#%d", prs[i].Repository, prs[i].Number)
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				all = append(all, prs[i])
			}
		}
	}
	if runtimeCacheEnabled(e) {
		renovatePRsCache.Set(cacheKey, all, 30*time.Second)
	}
	return all, nil
}

func searchRenovatePRsWith(e execer, query string) ([]RenovatePR, error) {
	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+renovatePRQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	return convertRenovatePRs(gqlResp.Data.Search.Nodes, gqlResp.Data.Viewer.Login), nil
}

// buildRenovateSearchQuery composes a GitHub issue-search query string.
//
// GitHub search treats parentheses and the `OR` keyword as text matches
// rather than boolean grouping, so wrapping qualifiers in `(a OR b)`
// silently returns zero results. Repeated same-name qualifiers act as an
// implicit OR — `author:a author:b repo:x repo:y` is the only form that
// works.
func buildRenovateSearchQuery(authors, repos []string) string {
	parts := []string{"is:pr", "is:open"}
	for _, author := range authors {
		if strings.TrimSpace(author) == "" {
			continue
		}
		parts = append(parts, "author:"+author)
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo) == "" {
			continue
		}
		parts = append(parts, "repo:"+repo)
	}
	return strings.Join(parts, " ")
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
					checks = append(checks, CheckRunInfo{
						Name:       ctx.Name,
						Status:     ctx.Status,
						Conclusion: ctx.Conclusion,
					})
				}
			}
		}

		prs = append(prs, RenovatePR{PullRequest: pr, CheckRuns: checks})
	}
	return prs
}
