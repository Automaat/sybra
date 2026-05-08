package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const issueQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 100) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      ... on Issue {
        number
        title
        body
        url
        state
        createdAt
        updatedAt
        author { login }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
      }
    }
  }
}`

const issueSnapshotQuery = `query($assignedQ: String!, $labeledQ: String!) {
  assigned: search(query: $assignedQ, type: ISSUE, first: 100) {
    nodes {
      ... on Issue {
        number
        title
        body
        url
        state
        createdAt
        updatedAt
        author { login }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
      }
    }
  }
  labeled: search(query: $labeledQ, type: ISSUE, first: 100) {
    nodes {
      ... on Issue {
        number
        title
        body
        url
        state
        createdAt
        updatedAt
        author { login }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
      }
    }
  }
}`

type gqlIssueResponse struct {
	Data struct {
		Search struct {
			Nodes    []gqlIssue `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlIssueSnapshotResponse struct {
	Data struct {
		Assigned struct {
			Nodes []gqlIssue `json:"nodes"`
		} `json:"assigned"`
		Labeled struct {
			Nodes []gqlIssue `json:"nodes"`
		} `json:"labeled"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

type IssueSnapshot struct {
	Assigned []Issue
	Labeled  []Issue
}

// FetchAssignedIssues returns open issues assigned to the authenticated user.
func FetchAssignedIssues() ([]Issue, error) {
	return fetchAssignedIssuesWith(defaultExecer)
}

func fetchAssignedIssuesWith(e execer) ([]Issue, error) {
	const query = "is:issue is:open assignee:@me sort:updated-desc"
	if runtimeCacheEnabled(e) {
		if cached, ok := assignedIssuesCache.Get(query); ok {
			return cached, nil
		}
		if ghGate.shouldSkipOptional("graphql") {
			if stale, ok := assignedIssuesCache.GetStale(query); ok {
				return stale, nil
			}
		}
	}

	issues, err := searchIssuesWith(e, query)
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := assignedIssuesCache.GetStale(query); ok {
				return stale, nil
			}
		}
		return nil, err
	}
	if runtimeCacheEnabled(e) {
		assignedIssuesCache.Set(query, issues, 30*time.Second)
	}
	return issues, nil
}

// FetchLabeledIssuesForRepos returns open issues with the given label across the specified repos.
func FetchLabeledIssuesForRepos(repos []string, label string) ([]Issue, error) {
	return fetchLabeledIssuesForReposWith(defaultExecer, repos, label)
}

func fetchLabeledIssuesForReposWith(e execer, repos []string, label string) ([]Issue, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = "repo:" + r
	}
	query := fmt.Sprintf("is:issue is:open label:%s %s sort:updated-desc", label, strings.Join(parts, " "))
	if runtimeCacheEnabled(e) {
		if cached, ok := labeledIssuesCache.Get(query); ok {
			return cached, nil
		}
		if ghGate.shouldSkipOptional("graphql") {
			if stale, ok := labeledIssuesCache.GetStale(query); ok {
				return stale, nil
			}
		}
	}

	issues, err := searchIssuesWith(e, query)
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := labeledIssuesCache.GetStale(query); ok {
				return stale, nil
			}
		}
		return nil, err
	}
	if runtimeCacheEnabled(e) {
		labeledIssuesCache.Set(query, issues, 30*time.Second)
	}
	return issues, nil
}

func searchIssuesWith(e execer, query string) ([]Issue, error) {
	httpResp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+issueQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(httpResp.body), err)
	}

	var gqlResp gqlIssueResponse
	if err := json.Unmarshal(httpResp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	return convertIssues(gqlResp.Data.Search.Nodes), nil
}

// FetchIssueSnapshot returns assigned issues and labeled issues in one GraphQL call.
func FetchIssueSnapshot(repos []string, label string) (IssueSnapshot, error) {
	return fetchIssueSnapshotWith(defaultExecer, repos, label)
}

func fetchIssueSnapshotWith(e execer, repos []string, label string) (IssueSnapshot, error) {
	snapshot := IssueSnapshot{}
	if len(repos) == 0 {
		assigned, err := fetchAssignedIssuesWith(e)
		if err != nil {
			return snapshot, err
		}
		snapshot.Assigned = assigned
		return snapshot, nil
	}

	labeledParts := make([]string, len(repos))
	for i, repo := range repos {
		labeledParts[i] = "repo:" + repo
	}
	assignedQuery := "is:issue is:open assignee:@me sort:updated-desc"
	labeledQuery := fmt.Sprintf("is:issue is:open label:%s %s sort:updated-desc", label, strings.Join(labeledParts, " "))

	if runtimeCacheEnabled(e) {
		if ghGate.shouldSkipOptional("graphql") {
			if assigned, ok := assignedIssuesCache.GetStale(assignedQuery); ok {
				snapshot.Assigned = assigned
			}
			if labeled, ok := labeledIssuesCache.GetStale(labeledQuery); ok {
				snapshot.Labeled = labeled
			}
			if snapshot.Assigned != nil || snapshot.Labeled != nil {
				return snapshot, nil
			}
		}
	}

	httpResp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+issueSnapshotQuery,
		"-f", "assignedQ="+assignedQuery,
		"-f", "labeledQ="+labeledQuery)
	if err != nil {
		if runtimeCacheEnabled(e) {
			if assigned, ok := assignedIssuesCache.GetStale(assignedQuery); ok {
				snapshot.Assigned = assigned
			}
			if labeled, ok := labeledIssuesCache.GetStale(labeledQuery); ok {
				snapshot.Labeled = labeled
			}
			if snapshot.Assigned != nil || snapshot.Labeled != nil {
				return snapshot, nil
			}
		}
		return snapshot, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(httpResp.body), err)
	}

	var gqlResp gqlIssueSnapshotResponse
	if err := json.Unmarshal(httpResp.body, &gqlResp); err != nil {
		return snapshot, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return snapshot, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	snapshot.Assigned = convertIssues(gqlResp.Data.Assigned.Nodes)
	snapshot.Labeled = convertIssues(gqlResp.Data.Labeled.Nodes)
	if runtimeCacheEnabled(e) {
		assignedIssuesCache.Set(assignedQuery, snapshot.Assigned, 30*time.Second)
		labeledIssuesCache.Set(labeledQuery, snapshot.Labeled, 30*time.Second)
	}
	return snapshot, nil
}

func convertIssues(nodes []gqlIssue) []Issue {
	issues := make([]Issue, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		// Skip PRs that sneak in (they are technically issues).
		if n.URL == "" || n.Number == 0 {
			continue
		}

		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}

		issues = append(issues, Issue{
			Number:     n.Number,
			Title:      n.Title,
			Body:       n.Body,
			URL:        n.URL,
			Repository: n.Repository.NameWithOwner,
			RepoName:   n.Repository.Name,
			Labels:     labels,
			Author:     n.Author.Login,
			CreatedAt:  n.CreatedAt,
			UpdatedAt:  n.UpdatedAt,
		})
	}
	return issues
}

// FetchIssue fetches a single issue by repo (owner/repo) and number.
func FetchIssue(repo string, number int) (Issue, error) {
	return fetchIssueWith(defaultExecer, repo, number)
}

func fetchIssueWith(e execer, repo string, number int) (Issue, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := issueCache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("issue", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "number,title,body,url,labels,author")
	if err != nil {
		return Issue{}, fmt.Errorf("gh issue view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Issue{}, fmt.Errorf("parse issue: %w", err)
	}
	labels := make([]string, len(raw.Labels))
	for i, l := range raw.Labels {
		labels[i] = l.Name
	}
	parts := strings.SplitN(repo, "/", 2)
	repoName := ""
	if len(parts) == 2 {
		repoName = parts[1]
	}
	issue := Issue{
		Number:     raw.Number,
		Title:      raw.Title,
		Body:       raw.Body,
		URL:        raw.URL,
		Repository: repo,
		RepoName:   repoName,
		Labels:     labels,
		Author:     raw.Author.Login,
	}
	if runtimeCacheEnabled(e) {
		issueCache.Set(key, issue, 2*time.Minute)
	}
	return issue, nil
}
