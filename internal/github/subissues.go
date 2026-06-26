package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// umbrellaQuery fetches an umbrella issue together with its native GitHub
// sub-issues in one round trip. The subIssues connection requires the
// sub_issues GraphQL feature, requested via the GraphQL-Features header.
const umbrellaQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    issue(number:$number){
      number title body url state
      repository{ name nameWithOwner }
      subIssues(first:100){
        nodes{
          number title body url state
          repository{ name nameWithOwner }
          labels(first:20){ nodes{ name } }
        }
      }
    }
  }
}`

type gqlUmbrellaResponse struct {
	Data struct {
		Repository struct {
			Issue *gqlUmbrellaIssue `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlUmbrellaIssue struct {
	gqlIssue
	SubIssues struct {
		Nodes []gqlIssue `json:"nodes"`
	} `json:"subIssues"`
}

// FetchUmbrella returns an umbrella issue and its GitHub sub-issues. repo is
// "owner/name". Sub-issues come back in the order GitHub lists them.
func FetchUmbrella(repo string, number int) (umbrella Issue, subs []Issue, err error) {
	return fetchUmbrellaWith(defaultExecer, repo, number)
}

func fetchUmbrellaWith(e execer, repo string, number int) (umbrella Issue, subs []Issue, err error) {
	owner, name, ok := splitOwnerRepo(repo)
	if !ok {
		return Issue{}, nil, fmt.Errorf("invalid repo %q (want owner/name)", repo)
	}
	httpResp, err := runGHAPIWith(e, "", "graphql",
		"-H", "GraphQL-Features: sub_issues",
		"-f", "query="+umbrellaQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", fmt.Sprintf("number=%d", number),
	)
	if err != nil {
		return Issue{}, nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(httpResp.body), err)
	}

	var resp gqlUmbrellaResponse
	if err := json.Unmarshal(httpResp.body, &resp); err != nil {
		return Issue{}, nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return Issue{}, nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	if resp.Data.Repository.Issue == nil {
		return Issue{}, nil, fmt.Errorf("issue %s#%d not found", repo, number)
	}

	node := resp.Data.Repository.Issue
	umbrellas := convertIssues([]gqlIssue{node.gqlIssue})
	if len(umbrellas) == 0 {
		return Issue{}, nil, fmt.Errorf("issue %s#%d is not a convertible issue", repo, number)
	}
	return umbrellas[0], convertIssues(node.SubIssues.Nodes), nil
}

// splitOwnerRepo splits "owner/name" into its parts. ok is false when either
// half is empty.
func splitOwnerRepo(repo string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
