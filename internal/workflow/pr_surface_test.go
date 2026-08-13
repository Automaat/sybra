package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/github"
)

// The point of the group is that a forgotten member is caught at wiring time.
// Before it, an omission left a nil field that turned the corresponding step
// into a silently skipped branch, discovered far from the omission.
func TestSetPRSurfaceNamesEveryMissingMember(t *testing.T) {
	t.Parallel()
	e := &Engine{}

	err := e.setPRSurfaceForTest(PRSurface{})
	if err == nil {
		t.Fatal("setPRSurfaceForTest accepted an entirely empty surface")
	}
	for _, want := range []string{
		"Linker", "ReviewRequester", "StateFetcher", "HeadFetcher", "MetaFetcher", "Creator",
		"Closer", "Finder", "AnyStateFinder", "ExistenceChecker", "ContentGenerator",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the missing %s", err, want)
		}
	}
}

// A partial surface is the realistic bug — one member forgotten among ten —
// so it must be rejected just as firmly as an empty one.
func TestSetPRSurfaceRejectsAPartialWiring(t *testing.T) {
	t.Parallel()
	full := completePRSurface()
	partial := full
	partial.Finder = nil

	err := (&Engine{}).setPRSurfaceForTest(partial)
	if err == nil {
		t.Fatal("setPRSurfaceForTest accepted a surface missing Finder")
	}
	if !strings.Contains(err.Error(), "Finder") {
		t.Errorf("error %q does not name Finder", err)
	}
	if strings.Contains(err.Error(), "Linker") {
		t.Errorf("error %q names Linker, which was supplied", err)
	}
}

func TestSetPRSurfaceWiresEveryField(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if err := e.setPRSurfaceForTest(completePRSurface()); err != nil {
		t.Fatalf("setPRSurfaceForTest(complete) = %v", err)
	}
	for name, got := range map[string]any{
		"prLinker":         e.pr.Linker,
		"prReviewers":      e.pr.ReviewRequester,
		"prStates":         e.pr.StateFetcher,
		"prHeads":          e.pr.HeadFetcher,
		"prMeta":           e.pr.MetaFetcher,
		"prCreator":        e.pr.Creator,
		"prCloser":         e.pr.Closer,
		"prFinder":         e.pr.Finder,
		"prAnyStateFinder": e.pr.AnyStateFinder,
		"prExistence":      e.pr.ExistenceChecker,
		"prContentGen":     e.pr.ContentGenerator,
	} {
		if got == nil {
			t.Errorf("%s is still nil after a complete setPRSurfaceForTest", name)
		}
	}
}

// stubPRSurface satisfies every PR interface with a do-nothing implementation.
// The group test cares only that a member is present, not what it returns.
type stubPRSurface struct{}

func (stubPRSurface) GetClosingIssues(string, int) (issues []int, body string, err error) {
	return nil, "", nil
}
func (stubPRSurface) EditBody(string, int, string) error                          { return nil }
func (stubPRSurface) RerequestReview(string, int) (reviewers []string, err error) { return nil, nil }
func (stubPRSurface) RequestCopilotReview(context.Context, string, int) error {
	return nil
}
func (stubPRSurface) FetchPRState(string, int) (github.PRState, error) { return github.PRState{}, nil }
func (stubPRSurface) FetchPRHeadSHA(context.Context, string, int) (string, error) {
	return "", nil
}
func (stubPRSurface) FetchPRMeta(context.Context, string, int) (github.PullRequest, error) {
	return github.PullRequest{}, nil
}
func (stubPRSurface) CreatePR(context.Context, string, PRCreateRequest) (number int, headSHA string, err error) {
	return 0, "", nil
}
func (stubPRSurface) ClosePR(context.Context, string, int, string) error { return nil }
func (stubPRSurface) FindPRForBranch(context.Context, string, string) (number int, found bool, err error) {
	return 0, false, nil
}
func (stubPRSurface) FindPRForBranchAnyState(context.Context, string, string) (number int, state string, found bool, err error) {
	return 0, "", false, nil
}
func (stubPRSurface) PRExists(context.Context, string, int) (bool, error) { return false, nil }
func (stubPRSurface) GeneratePRContent(context.Context, string, string, []string) (title, body string, err error) {
	return "", "", nil
}

func completePRSurface() PRSurface {
	s := stubPRSurface{}
	return PRSurface{
		Linker:           s,
		ReviewRequester:  s,
		StateFetcher:     s,
		HeadFetcher:      s,
		MetaFetcher:      s,
		Creator:          s,
		Closer:           s,
		Finder:           s,
		AnyStateFinder:   s,
		ExistenceChecker: s,
		ContentGenerator: s,
	}
}
