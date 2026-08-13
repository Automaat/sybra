// Package workflowpr wires the workflow engine's pull-request surface
// (workflow.PRSurface) to the github and prcontent packages. Extracted from
// internal/sybra: every type here is stateless or holds only its own
// dependencies, with zero coupling back into internal/sybra.
package workflowpr

import (
	"context"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/prcontent"
	"github.com/Automaat/sybra/internal/workflow"
)

// Compile-time interface checks.
var (
	_ workflow.PRLinker           = (*LinkerAdapter)(nil)
	_ workflow.PRStateFetcher     = (*StateFetcherAdapter)(nil)
	_ workflow.PRHeadFetcher      = (*HeadFetcherAdapter)(nil)
	_ workflow.PRMetaFetcher      = (*MetaFetcherAdapter)(nil)
	_ workflow.PRCreator          = (*CreatorAdapter)(nil)
	_ workflow.PRCloser           = (*CloserAdapter)(nil)
	_ workflow.PRFinder           = (*FinderAdapter)(nil)
	_ workflow.PRExistenceChecker = (*ExistenceCheckerAdapter)(nil)
	_ workflow.PRContentGenerator = (*ContentGeneratorAdapter)(nil)
	_ workflow.PRReviewRequester  = (*ReviewRequesterAdapter)(nil)
)

// LinkerAdapter wires the workflow engine's PRLinker interface to the github
// package. Stateless — all state lives in `gh` / GitHub.
type LinkerAdapter struct{}

func (LinkerAdapter) GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error) {
	return github.FetchPRClosingIssues(repo, prNumber)
}

func (LinkerAdapter) EditBody(repo string, prNumber int, body string) error {
	return github.EditPRBody(repo, prNumber, body)
}

// StateFetcherAdapter wires the workflow engine's PRStateFetcher interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type StateFetcherAdapter struct{}

func (StateFetcherAdapter) FetchPRState(repo string, number int) (github.PRState, error) {
	return github.FetchPRState(repo, number)
}

// HeadFetcherAdapter wires the workflow engine's PRHeadFetcher interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type HeadFetcherAdapter struct{}

func (HeadFetcherAdapter) FetchPRHeadSHA(ctx context.Context, repo string, number int) (string, error) {
	return github.FetchPRHeadSHAContext(ctx, repo, number)
}

// MetaFetcherAdapter wires the workflow engine's PRMetaFetcher interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type MetaFetcherAdapter struct{}

func (MetaFetcherAdapter) FetchPRMeta(ctx context.Context, repo string, number int) (github.PullRequest, error) {
	return github.FetchPRMetaContext(ctx, repo, number)
}

// CreatorAdapter wires the workflow engine's PRCreator interface to the
// github package. Stateless — all state lives in `gh` / GitHub.
type CreatorAdapter struct{}

func (CreatorAdapter) CreatePR(ctx context.Context, dir string, req workflow.PRCreateRequest) (number int, headSHA string, err error) {
	return github.CreatePR(ctx, dir, github.CreatePRRequest{
		Repo:  req.Repo,
		Head:  req.Head,
		Draft: req.Draft,
		Title: req.Title,
		Body:  req.Body,
	})
}

// CloserAdapter wires the workflow engine's best-effort superseded-PR
// cleanup to the github package.
type CloserAdapter struct{}

func (CloserAdapter) ClosePR(ctx context.Context, repo string, number int, comment string) error {
	return github.ClosePR(ctx, repo, number, comment)
}

// FinderAdapter wires the workflow engine's PRFinder interface to the github
// package. Stateless — all state lives in `gh` / GitHub.
type FinderAdapter struct{}

func (FinderAdapter) FindPRForBranch(ctx context.Context, repo, head string) (number int, found bool, err error) {
	return github.FindPRForBranch(ctx, repo, head)
}

func (FinderAdapter) FindPRForBranchAnyState(ctx context.Context, repo, head string) (number int, state string, found bool, err error) {
	return github.FindPRForBranchAnyState(ctx, repo, head)
}

// ExistenceCheckerAdapter wires the workflow engine's PRExistenceChecker
// interface to the github package. Stateless — all state lives in `gh` /
// GitHub.
type ExistenceCheckerAdapter struct{}

func (ExistenceCheckerAdapter) PRExists(ctx context.Context, repo string, number int) (bool, error) {
	return github.PRExists(ctx, repo, number)
}

// ContentGeneratorAdapter wires the workflow engine's PRContentGenerator
// interface to internal/prcontent's LLM-backed drafter.
type ContentGeneratorAdapter struct {
	Gen prcontent.Generator
}

func (a ContentGeneratorAdapter) GeneratePRContent(ctx context.Context, taskTitle, taskBody string, commitSubjects []string) (title, body string, err error) {
	c, err := a.Gen.Generate(ctx, prcontent.Request{
		TaskTitle:      taskTitle,
		TaskBody:       taskBody,
		CommitSubjects: commitSubjects,
	})
	if err != nil {
		return "", "", err
	}
	return c.Title, c.Body, nil
}

// ReviewRequesterAdapter asks users who left actionable PR feedback to
// review again after the fix-review workflow pushes updated commits.
type ReviewRequesterAdapter struct{}

func (ReviewRequesterAdapter) RerequestReview(repo string, prNumber int) ([]string, error) {
	ctx, err := github.FetchPRContext(repo, prNumber)
	if err != nil {
		return nil, err
	}
	viewer := github.ViewerLogin()
	seen := map[string]struct{}{}
	reviewers := make([]string, 0, len(ctx.Comments))
	for _, c := range ctx.Comments {
		login := strings.TrimSpace(c.Author)
		if !EligibleRerequestReviewer(login, viewer, ctx.Author) {
			continue
		}
		key := strings.ToLower(login)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		reviewers = append(reviewers, login)
	}
	if len(reviewers) == 0 {
		return nil, nil
	}
	if err := github.RequestReviewers(repo, prNumber, reviewers); err != nil {
		return nil, err
	}
	return reviewers, nil
}

func (ReviewRequesterAdapter) RequestCopilotReview(ctx context.Context, repo string, prNumber int) error {
	return github.RequestCopilotReviewCtx(ctx, repo, prNumber)
}

// EligibleRerequestReviewer reports whether login should be asked to
// re-review: not empty, not the viewer or PR author, and not a bot account.
func EligibleRerequestReviewer(login, viewer, prAuthor string) bool {
	if login == "" {
		return false
	}
	if strings.EqualFold(login, viewer) || strings.EqualFold(login, prAuthor) {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(login), "[bot]")
}
