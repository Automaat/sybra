package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
)

type fakeCICheckGetter struct {
	fakeCheckGetter
	policy *project.CIConfig
	err    error
	reads  int
}

func (f *fakeCICheckGetter) CIConfig(context.Context, string) (*project.CIConfig, error) {
	f.reads++
	return f.policy, f.err
}

func enableTestCI(e *Engine) {
	e.setCheckConfigGetterForTest(&fakeCICheckGetter{policy: &project.CIConfig{Enabled: true, RequiredChecks: []string{"tests"}}})
}

type readyPRCreator struct {
	fakePRCreator
	ready int
}

func (f *readyPRCreator) MarkReady(context.Context, string, int) error {
	f.ready++
	return nil
}

func TestStartCIDraftNeverLinksBeforeLocalGates(t *testing.T) {
	for _, lookup := range []string{"create", "open", "any-state", "creation-conflict"} {
		t.Run(lookup, func(t *testing.T) {
			_, wt := newPRWorktree(t, "feat/early-ci")
			commitFile(t, wt, "change.txt", "change")
			tasks := newMemTasks()
			task := TaskInfo{ID: "t1", Status: "in-progress", Branch: "feat/early-ci", ProjectID: "o/r", ProjectType: "pet"}
			tasks.Put(task)
			e := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
			e.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
			enableTestCI(e)
			creator := &fakePRCreator{number: 42, headSHA: headSHA(t, wt)}
			e.SetPRCreator(creator)
			switch lookup {
			case "open":
				e.SetPRFinder(&fakePRFinder{number: 42, found: true})
			case "any-state":
				e.SetPRFinder(&fakePRFinder{err: errors.New("temporary lookup failure")})
				e.setPRAnyStateFinderForTest(&fakePRAnyStateFinder{number: 42, state: "OPEN", found: true})
			case "creation-conflict":
				e.SetPRFinder(&raceThenFoundFinder{})
				creator.err = errors.New("a pull request already exists")
			}
			out, err := e.execStartCI("t1", &Step{ID: "start_ci", Type: StepStartCI}, &Execution{}, task)
			if err != nil || out.Status != "completed" {
				t.Fatalf("start CI: %+v, %v", out, err)
			}
			got, _ := tasks.GetTask("t1")
			if got.PRNumber != 0 || got.Status != task.Status {
				t.Fatalf("early lookup bypassed local gates: %+v", got)
			}
			if lookup == "create" && !creator.gotReq.Draft {
				t.Fatal("early PR was not a draft")
			}
			if !strings.HasPrefix(remoteBranchSHA(t, wt, task.Branch), headSHA(t, wt)) {
				t.Fatal("early CI branch was not pushed")
			}
		})
	}
}

func TestCIPolicyUnavailableNeverSkipsVerification(t *testing.T) {
	e := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	getter := &fakeCICheckGetter{err: errors.New("unreadable trusted policy")}
	e.setCheckConfigGetterForTest(getter)
	if _, err := e.execStartCI("t1", &Step{ID: "start_ci"}, &Execution{}, TaskInfo{}); err == nil || getter.reads != 1 {
		t.Fatalf("start did not use one error-bearing snapshot: reads=%d err=%v", getter.reads, err)
	}
	if !e.ciEnabled("t1") {
		t.Fatal("unavailable policy relaxed evidence requirements")
	}
	if _, err := e.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{}); err == nil {
		t.Fatal("unavailable policy bypassed evidence gate")
	}
}

func TestCIReviewCacheBindsDispatchRevisionAndHydratedContract(t *testing.T) {
	_, wt := newPRWorktree(t, "feat/review-cache")
	e, tasks, rec := newRequireEvidenceEngine(t, wt)
	enableTestCI(e)
	e.setManualTestConfigGetterForTest(fakeManualTestConfigGetter{"t1": {Kind: "server", Command: "start isolated server"}})
	task := TaskInfo{ID: "t1", Title: "contract", Body: "behavior", Reviewed: true, CodeReviewVerdict: "CLEAN", Workflow: &Execution{}}
	step := &Step{ID: "review", Config: StepConfig{Role: reviewAgentRole}}
	tasks.Put(task)
	if err := e.bindVerificationInput("t1", step, task.Workflow, e.withManualTestConfig(task)); err != nil {
		t.Fatal(err)
	}
	e.recordEvidence("t1", "review", evidenceCriterionReview, evidence.ProofReviewFinding, 0, "", "CLEAN")
	if !e.reusableReview(task) {
		t.Fatal("unchanged input with hydrated manual test missed review cache")
	}
	changed := task
	changed.Body = "new requirement"
	if e.reusableReview(changed) {
		t.Fatal("changed task contract reused old review")
	}
	before, _ := rec.Evidence("t1")
	commitFile(t, wt, "fix.txt", "fix after review")
	e.refreshReviewEvidenceFreshness("t1")
	after, _ := rec.Evidence("t1")
	if e.reusableReview(task) || after.Criteria[0].FinalRev != before.Criteria[0].FinalRev {
		t.Fatal("new revision reused or relabeled old review")
	}
}

func TestCIDraftPublicationRequiresHeadAndEvidenceButOrdinaryFixPushDoesNot(t *testing.T) {
	_, wt := newPRWorktree(t, "feat/publish")
	e, tasks, rec := newRequireEvidenceEngine(t, wt)
	enableTestCI(e)
	task := TaskInfo{ID: "t1", Status: "ready-pr", ProjectID: "o/r", ProjectType: "pet"}
	tasks.Put(task)
	creator := &readyPRCreator{}
	e.SetPRCreator(creator)
	meta := &fakePRMetaFetcher{pr: github.PullRequest{IsDraft: true, HeadSHA: "stale"}}
	e.setPRMetaFetcherForTest(meta)
	if err := e.publishCIDraft("t1", task, 42); err == nil || creator.ready != 0 {
		t.Fatal("draft published against stale PR head")
	}
	meta.pr.HeadSHA = headSHA(t, wt)
	if err := e.publishCIDraft("t1", task, 42); err == nil || creator.ready != 0 {
		t.Fatal("draft published without completion evidence")
	}
	tasks.Put(task)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, FinalRev: meta.pr.HeadSHA},
		{Criterion: evidenceCriterionDetectTampering, FinalRev: meta.pr.HeadSHA},
	}})
	if err := e.publishCIDraft("t1", task, 42); err != nil || creator.ready != 1 {
		t.Fatalf("verified draft did not publish: %v", err)
	}
	meta.pr.IsDraft = false
	meta.pr.HeadSHA = "post-fix-head"
	rec.SetLoadErr(errors.New("old proof does not authorize new fix"))
	if err := e.publishCIDraft("t1", task, 42); err != nil || creator.ready != 1 {
		t.Fatalf("ordinary fix push incorrectly ran publication gate: %v", err)
	}
}
