package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const maxCheckContextPages = 20

const checkContextsPageQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      commits(last: 1) {
        nodes {
          commit {
            statusCheckRollup {
              contexts(first: 100, after: $after) {
                pageInfo { hasNextPage endCursor }
                nodes {
                  __typename
                  ... on CheckRun {
                    name
                    status
                    conclusion
                    startedAt
                    completedAt
                    checkSuite { workflowRun { id runAttempt } }
                  }
                  ... on StatusContext { name: context state }
                }
              }
            }
          }
        }
      }
    }
  }
}`

// completePRCheckContexts follows the nested statusCheckRollup connection for
// PRs whose initial bounded query was truncated. CI status is recomputed from
// the resulting complete set, so a late matrix failure cannot be hidden by an
// earlier page of successes. A failed continuation fails the whole PR fetch:
// callers retry rather than certifying an incomplete rollup as green.
func completePRCheckContexts(ctx context.Context, e execer, prs []gqlPR) error {
	for i := range prs {
		if err := completePRCheckContext(ctx, &prs[i], e); err != nil {
			return err
		}
	}
	return nil
}

func completePRCheckContext(ctx context.Context, pr *gqlPR, e execer) error {
	if pr == nil || len(pr.Commits.Nodes) == 0 {
		return nil
	}
	rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup
	if rollup == nil || !rollup.Contexts.PageInfo.HasNextPage {
		return nil
	}
	owner, name, ok := strings.Cut(pr.Repository.NameWithOwner, "/")
	if !ok || owner == "" || name == "" || pr.Number <= 0 {
		return fmt.Errorf("paginate check contexts: invalid PR identity %q#%d", pr.Repository.NameWithOwner, pr.Number)
	}

	for range maxCheckContextPages {
		cursor := rollup.Contexts.PageInfo.EndCursor
		if cursor == "" {
			return fmt.Errorf("paginate check contexts for %s#%d: next page has no cursor", pr.Repository.NameWithOwner, pr.Number)
		}
		page, err := fetchCheckContextPage(ctx, e, owner, name, pr.Number, cursor)
		if err != nil {
			return fmt.Errorf("paginate check contexts for %s#%d: %w", pr.Repository.NameWithOwner, pr.Number, err)
		}
		rollup.Contexts.Nodes = append(rollup.Contexts.Nodes, page.Nodes...)
		rollup.Contexts.PageInfo = page.PageInfo
		if !page.PageInfo.HasNextPage {
			return nil
		}
	}
	return fmt.Errorf("paginate check contexts for %s#%d: exceeded %d pages", pr.Repository.NameWithOwner, pr.Number, maxCheckContextPages)
}

func fetchCheckContextPage(ctx context.Context, e execer, owner, name string, number int, cursor string) (gqlCheckContextConnection, error) {
	var out struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					Commits struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	resp, err := runGHAPICtxWith(ctx, e, "", "graphql",
		"-f", "query="+checkContextsPageQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", "number="+strconv.Itoa(number),
		"-f", "after="+cursor)
	if err != nil {
		return gqlCheckContextConnection{}, err
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return gqlCheckContextConnection{}, fmt.Errorf("parse response: %w", err)
	}
	if len(out.Errors) > 0 {
		return gqlCheckContextConnection{}, fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}
	if out.Data.Repository.PullRequest == nil || len(out.Data.Repository.PullRequest.Commits.Nodes) == 0 ||
		out.Data.Repository.PullRequest.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return gqlCheckContextConnection{}, fmt.Errorf("missing pull request check rollup")
	}
	return out.Data.Repository.PullRequest.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts, nil
}
