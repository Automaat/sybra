package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ReviewThread is one PR review conversation thread with the signals needed to
// decide whether a Copilot-authored thread has been addressed and can be
// auto-resolved.
type ReviewThread struct {
	ID              string
	AuthorLogin     string // login of the thread's first comment author
	LastAuthorLogin string // login of the thread's most recent comment author
	IsResolved      bool
	IsOutdated      bool   // the anchored code changed since the comment — i.e. addressed
	Path            string // file the thread is anchored to, empty for a file-level thread
	Line            int    // line the thread is anchored to, 0 when the anchor is gone
	CommentCount    int    // total comments on the thread, the monotone "was it answered" signal
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments { totalCount }
          first: comments(first: 1) { nodes { author { login } } }
          last: comments(last: 1) { nodes { author { login } } }
        }
      }
    }
  }
}`

type gqlReviewThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Comments   struct {
							TotalCount int `json:"totalCount"`
						} `json:"comments"`
						First struct {
							Nodes []struct {
								Author struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"first"`
						Last struct {
							Nodes []struct {
								Author struct {
									Login string `json:"login"`
								} `json:"author"`
							} `json:"nodes"`
						} `json:"last"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// FetchReviewThreads returns a PR's review threads — up to the first 100, which
// covers any realistic PR — with each thread's author and resolution state. The
// query is not paginated (matching the rest of this package); on the rare PR
// with more than 100 threads the overflow is ignored, so an addressed Copilot
// thread beyond the first 100 would not be auto-resolved and that PR would need
// a manual merge.
func FetchReviewThreads(repo string, number int) ([]ReviewThread, error) {
	return fetchReviewThreadsWith(context.Background(), defaultExecer, repo, number)
}

// FetchReviewThreadsContext is FetchReviewThreads bound to a caller's context,
// for the dispatch paths that already carry one.
func FetchReviewThreadsContext(ctx context.Context, repo string, number int) ([]ReviewThread, error) {
	return fetchReviewThreadsWith(ctx, defaultExecer, repo, number)
}

func fetchReviewThreadsWith(ctx context.Context, e execer, repo string, number int) ([]ReviewThread, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("invalid repo %q", repo)
	}
	resp, err := runGHAPICtxWith(ctx, e, "", "graphql",
		"-f", "query="+reviewThreadsQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlReviewThreadsResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	nodes := gqlResp.Data.Repository.PullRequest.ReviewThreads.Nodes
	threads := make([]ReviewThread, 0, len(nodes))
	for i := range nodes {
		var firstLogin, lastLogin string
		if len(nodes[i].First.Nodes) > 0 {
			firstLogin = nodes[i].First.Nodes[0].Author.Login
		}
		if len(nodes[i].Last.Nodes) > 0 {
			lastLogin = nodes[i].Last.Nodes[0].Author.Login
		}
		threads = append(threads, ReviewThread{
			ID:              nodes[i].ID,
			AuthorLogin:     firstLogin,
			LastAuthorLogin: lastLogin,
			IsResolved:      nodes[i].IsResolved,
			IsOutdated:      nodes[i].IsOutdated,
			Path:            nodes[i].Path,
			Line:            nodes[i].Line,
			CommentCount:    nodes[i].Comments.TotalCount,
		})
	}
	return threads, nil
}

const resolveReviewThreadMutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id }
  }
}`

// ResolveReviewThread marks a PR review thread resolved. Resolving an
// already-resolved thread is a no-op, so the call is safe to retry.
func ResolveReviewThread(threadID string) error {
	return resolveReviewThreadWith(defaultExecer, threadID)
}

func resolveReviewThreadWith(e execer, threadID string) error {
	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+resolveReviewThreadMutation,
		"-f", "threadId="+threadID)
	if err != nil {
		return fmt.Errorf("gh api graphql resolve thread: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}
	return nil
}
