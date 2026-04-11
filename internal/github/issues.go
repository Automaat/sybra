package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

const issueQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
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
			Nodes []gqlIssue `json:"nodes"`
		} `json:"search"`
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

// FetchAssignedIssues returns open issues assigned to the authenticated user.
func FetchAssignedIssues() ([]Issue, error) {
	return fetchAssignedIssuesWith(defaultExecer)
}

func fetchAssignedIssuesWith(e execer) ([]Issue, error) {
	return searchIssuesWith(e, "is:issue is:open assignee:@me sort:updated-desc")
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
	return searchIssuesWith(e, query)
}

func searchIssuesWith(e execer, query string) ([]Issue, error) {
	out, err := e.run("api", "graphql",
		"-f", "query="+issueQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp gqlIssueResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	return convertIssues(resp.Data.Search.Nodes), nil
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
	return Issue{
		Number:     raw.Number,
		Title:      raw.Title,
		Body:       raw.Body,
		URL:        raw.URL,
		Repository: repo,
		RepoName:   repoName,
		Labels:     labels,
		Author:     raw.Author.Login,
	}, nil
}
