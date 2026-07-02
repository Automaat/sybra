package github

import (
	"encoding/json"
	"fmt"
	"strconv"
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

const issueLinkedPRsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      timelineItems(first: 100, itemTypes: CROSS_REFERENCED_EVENT) {
        nodes {
          ... on CrossReferencedEvent {
            willCloseTarget
            source {
              ... on PullRequest {
                number
                title
                url
                state
                headRefName
                author { login type: __typename }
                repository { name nameWithOwner }
              }
            }
          }
        }
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

type gqlIssueLinkedPRsResponse struct {
	Data struct {
		Repository struct {
			Issue struct {
				TimelineItems struct {
					Nodes []gqlCrossReferencedEvent `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlCrossReferencedEvent struct {
	WillCloseTarget bool `json:"willCloseTarget"`
	Source          struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		State       string `json:"state"`
		HeadRefName string `json:"headRefName"`
		Author      struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"author"`
		Repository struct {
			Name          string `json:"name"`
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"source"`
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
		if ghGate.shouldSkipOptional("graphql", priorityDiscovery) {
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
	// Cache against the full repo set; the per-chunk searches below are an
	// implementation detail callers shouldn't see.
	cacheKey := "label:" + label + "||" + strings.Join(repos, ",")
	if runtimeCacheEnabled(e) {
		if cached, ok := labeledIssuesCache.Get(cacheKey); ok {
			return cached, nil
		}
		if ghGate.shouldSkipOptional("graphql", priorityDiscovery) {
			if stale, ok := labeledIssuesCache.GetStale(cacheKey); ok {
				return stale, nil
			}
		}
	}

	issues, err := searchLabeledChunked(e, repos, label)
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := labeledIssuesCache.GetStale(cacheKey); ok {
				return stale, nil
			}
		}
		return nil, err
	}
	if runtimeCacheEnabled(e) {
		labeledIssuesCache.Set(cacheKey, issues, 30*time.Second)
	}
	return issues, nil
}

// labeledRepoQuery builds the labeled-issue search query for one repo chunk.
func labeledRepoQuery(repos []string, label string) string {
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = "repo:" + r
	}
	return fmt.Sprintf("is:issue is:open label:%s %s sort:updated-desc", label, strings.Join(parts, " "))
}

// searchLabeledChunked runs the labeled-issue search in repo chunks so the
// query string can't grow unbounded (GitHub rejects/truncates oversized search
// queries with many repo: qualifiers), then dedupes by URL.
func searchLabeledChunked(e execer, repos []string, label string) ([]Issue, error) {
	seen := make(map[string]struct{})
	var all []Issue
	for start := 0; start < len(repos); start += issueSearchChunkSize {
		end := min(start+issueSearchChunkSize, len(repos))
		issues, err := searchIssuesWith(e, labeledRepoQuery(repos[start:end], label))
		if err != nil {
			return nil, err
		}
		for i := range issues {
			if _, dup := seen[issues[i].URL]; dup {
				continue
			}
			seen[issues[i].URL] = struct{}{}
			all = append(all, issues[i])
		}
	}
	return all, nil
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

// issueSearchChunkSize bounds how many repo: qualifiers go into one labeled
// search query, so a large project set can't produce an oversized query that
// GitHub rejects or silently truncates.
const issueSearchChunkSize = 15

// FetchIssueSnapshot returns assigned issues and labeled issues. The first repo
// chunk's labeled search rides the same GraphQL request as the assigned search;
// extra chunks are fetched as labeled-only searches and merged.
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

	// The combined assigned+labeled request only carries the first repo chunk's
	// labeled qualifiers, so the query can't grow unbounded with many projects.
	// Any remaining chunks are fetched as labeled-only searches below and merged.
	firstChunkEnd := min(issueSearchChunkSize, len(repos))
	assignedQuery := "is:issue is:open assignee:@me sort:updated-desc"
	labeledQuery := labeledRepoQuery(repos[:firstChunkEnd], label)
	// Cache labeled results against the full repo set, not just the first
	// chunk, so the stale-fallback returns the complete labeled list.
	labeledCacheKey := "label:" + label + "||" + strings.Join(repos, ",")

	if runtimeCacheEnabled(e) {
		if ghGate.shouldSkipOptional("graphql", priorityDiscovery) {
			if assigned, ok := assignedIssuesCache.GetStale(assignedQuery); ok {
				snapshot.Assigned = assigned
			}
			if labeled, ok := labeledIssuesCache.GetStale(labeledCacheKey); ok {
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
			if labeled, ok := labeledIssuesCache.GetStale(labeledCacheKey); ok {
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

	// Merge labeled issues from any repo chunks beyond the first (fetched as
	// standalone labeled-only searches to keep each query bounded).
	if len(repos) > firstChunkEnd {
		rest, err := searchLabeledChunked(e, repos[firstChunkEnd:], label)
		if err != nil {
			return snapshot, fmt.Errorf("fetch labeled issues (chunked): %w", err)
		}
		seen := make(map[string]struct{}, len(snapshot.Labeled))
		for i := range snapshot.Labeled {
			seen[snapshot.Labeled[i].URL] = struct{}{}
		}
		for i := range rest {
			if _, dup := seen[rest[i].URL]; !dup {
				snapshot.Labeled = append(snapshot.Labeled, rest[i])
			}
		}
	}

	if runtimeCacheEnabled(e) {
		assignedIssuesCache.Set(assignedQuery, snapshot.Assigned, 30*time.Second)
		labeledIssuesCache.Set(labeledCacheKey, snapshot.Labeled, 30*time.Second)
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
			State:      n.State,
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

	out, err := e.run("issue", "view", strconv.Itoa(number),
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

// FetchIssueLinkedPRs returns same-repo open PRs that GitHub links as closing
// the issue through cross-reference timeline events.
func FetchIssueLinkedPRs(repo string, issueNumber int) ([]PullRequest, error) {
	return fetchIssueLinkedPRsWith(defaultExecer, repo, issueNumber)
}

func fetchIssueLinkedPRsWith(e execer, repo string, issueNumber int) ([]PullRequest, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("invalid repo %q", repo)
	}
	httpResp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+issueLinkedPRsQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", fmt.Sprintf("number=%d", issueNumber))
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(httpResp.body), err)
	}

	var gqlResp gqlIssueLinkedPRsResponse
	if err := json.Unmarshal(httpResp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	events := gqlResp.Data.Repository.Issue.TimelineItems.Nodes
	prs := make([]PullRequest, 0, len(events))
	seen := make(map[int]struct{}, len(events))
	for i := range events {
		ev := &events[i]
		src := &ev.Source
		if !ev.WillCloseTarget || src.Number == 0 || src.State != "OPEN" || src.Repository.NameWithOwner != repo {
			continue
		}
		if _, ok := seen[src.Number]; ok {
			continue
		}
		seen[src.Number] = struct{}{}
		prs = append(prs, PullRequest{
			Number:      src.Number,
			Title:       src.Title,
			URL:         src.URL,
			HeadRefName: src.HeadRefName,
			Repository:  src.Repository.NameWithOwner,
			RepoName:    src.Repository.Name,
			Author:      src.Author.Login,
		})
	}
	return prs, nil
}
