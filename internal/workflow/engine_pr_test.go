package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestExecRerequestReview_NoRequesterSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "no pr review requester") {
		t.Errorf("Output = %q, want no requester skip", out.Output)
	}
}

func TestExecRerequestReview_MissingFieldsSkip(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	requester := &fakePRReviewRequester{}
	engine.setPRReviewRequesterForTest(requester)

	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), TaskInfo{ID: "t1", ProjectID: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 0 {
		t.Fatalf("requester calls = %d, want 0", requester.calls)
	}
	if !strings.Contains(out.Output, "missing pr or project") {
		t.Errorf("Output = %q, want missing fields skip", out.Output)
	}
}

func TestExecRerequestReview_RequestsReviewers(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	requester := &fakePRReviewRequester{reviewers: []string{"alice", "bob"}}
	engine.setPRReviewRequesterForTest(requester)

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if requester.calls != 1 || requester.repo != "owner/repo" || requester.prNumber != 5 {
		t.Fatalf("requester = calls:%d repo:%q pr:%d", requester.calls, requester.repo, requester.prNumber)
	}
	if !strings.Contains(out.Output, "@alice") || !strings.Contains(out.Output, "@bob") {
		t.Errorf("Output = %q, want requested reviewers", out.Output)
	}
}

func TestExecRerequestReview_ErrorIsNonFatal(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.setPRReviewRequesterForTest(&fakePRReviewRequester{err: fmt.Errorf("boom")})

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}
	out, err := engine.execRerequestReview("t1", newRerequestReviewStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "request failed") {
		t.Errorf("output = %+v, want completed failure note", out)
	}
}

// TestExecEnsurePRClosesIssue tables the execEnsurePRClosesIssue outcomes
// that share the same store/tasks/agents/engine/linker setup and differ only
// in the linker's canned responses and what's asserted afterward.
func TestExecEnsurePRClosesIssue(t *testing.T) {
	baseTI := func() TaskInfo {
		return TaskInfo{
			ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
			Issue: "https://github.com/owner/repo/issues/7",
		}
	}

	cases := []struct {
		name          string
		noLinker      bool
		linker        *fakePRLinker
		ti            TaskInfo
		taskStatus    taskstatus.Status // "" = don't seed tasks.Put
		wantOutStatus string            // defaults to "completed"
		check         func(t *testing.T, out StepOutput, linker *fakePRLinker)
		checkStatus   taskstatus.Status // "" = don't re-check task status after
	}{
		{
			name:     "NoLinkerSkips",
			noLinker: true,
			ti:       baseTI(),
			check: func(t *testing.T, out StepOutput, _ *fakePRLinker) {
				if !strings.Contains(out.Output, "no pr linker") {
					t.Errorf("Output = %q, want 'no pr linker' skip reason", out.Output)
				}
			},
		},
		{
			name:   "CrossRepoSkips",
			linker: &fakePRLinker{},
			ti: TaskInfo{
				ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
				Issue: "https://github.com/other/elsewhere/issues/7",
			},
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if !strings.Contains(out.Output, "cross-repo") {
					t.Errorf("Output = %q, want cross-repo skip", out.Output)
				}
				if linker.getCalls != 0 {
					t.Errorf("GetClosingIssues called %d times, want 0 (skip before fetch)", linker.getCalls)
				}
			},
		},
		{
			name: "AlreadyLinkedNoEdit",
			linker: &fakePRLinker{
				getQueue: []getResult{{issues: []int{7}, body: "original"}},
			},
			ti: baseTI(),
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if !strings.Contains(out.Output, "already linked") {
					t.Errorf("Output = %q, want already linked", out.Output)
				}
				if linker.editCalls != 0 {
					t.Errorf("EditBody called %d times, want 0", linker.editCalls)
				}
			},
		},
		{
			name: "EditAppendsAndVerifies",
			linker: &fakePRLinker{
				getQueue: []getResult{
					{issues: nil, body: "Implements the feature."},
					{issues: []int{7}, body: "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"},
				},
			},
			ti:          baseTI(),
			taskStatus:  "in-review",
			checkStatus: "in-review", // unchanged on success
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if linker.editCalls != 1 {
					t.Errorf("EditBody called %d times, want 1", linker.editCalls)
				}
				wantBody := "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"
				if linker.lastBody != wantBody {
					t.Errorf("edit body = %q, want %q", linker.lastBody, wantBody)
				}
			},
		},
		{
			name: "EmptyBodyNoLeadingNewlines",
			linker: &fakePRLinker{
				getQueue: []getResult{
					{issues: nil, body: ""},
					{issues: []int{7}, body: "Closes https://github.com/owner/repo/issues/7"},
				},
			},
			ti: baseTI(),
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if linker.lastBody != "Closes https://github.com/owner/repo/issues/7" {
					t.Errorf("edit body = %q, want no leading newlines", linker.lastBody)
				}
			},
		},
		{
			name: "EditFailureFlipsHumanRequired",
			linker: &fakePRLinker{
				getQueue: []getResult{{issues: nil, body: "body"}},
				editErr:  fmt.Errorf("403 forbidden"),
			},
			ti:            baseTI(),
			taskStatus:    "in-review",
			wantOutStatus: "failed",
			checkStatus:   "human-required",
		},
		{
			// Verification lag is a false negative: gh pr edit succeeded, the
			// body contains "Closes <url>", but GitHub hasn't re-parsed
			// closingIssuesReferences yet. The step must trust the body and
			// leave the task status alone instead of flipping to
			// human-required.
			name: "VerifyLagTrustsBody",
			linker: &fakePRLinker{
				getQueue: []getResult{
					// 1 pre-check + 4 verify attempts, all miss.
					{issues: nil, body: "body"},
					{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
					{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
					{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
					{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
				},
			},
			ti:          baseTI(),
			taskStatus:  "in-review",
			checkStatus: "in-review",
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if !strings.Contains(out.Output, "trusting body") {
					t.Errorf("Output = %q, want 'trusting body' message", out.Output)
				}
				// 1 pre-check + 1 initial verify + 3 retries = 5 fetches.
				if linker.getCalls != 5 {
					t.Errorf("GetClosingIssues calls = %d, want 5 (pre-check + 4 verify attempts)", linker.getCalls)
				}
			},
		},
		{
			// Verification should retry: first post-edit fetch misses
			// (GitHub lagging), second fetch sees the parsed closing
			// reference.
			name: "VerifyRetrySucceeds",
			linker: &fakePRLinker{
				getQueue: []getResult{
					{issues: nil, body: "body"},                    // pre-check miss → triggers edit
					{issues: nil, body: "body\n\nCloses ..."},      // verify attempt 0: still stale
					{issues: []int{7}, body: "body\n\nCloses ..."}, // verify attempt 1: parsed
				},
			},
			ti:         baseTI(),
			taskStatus: "in-review",
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if !strings.Contains(out.Output, "linked issue #7") {
					t.Errorf("Output = %q, want linked issue #7", out.Output)
				}
				if linker.getCalls != 3 {
					t.Errorf("GetClosingIssues calls = %d, want 3 (pre-check + 2 verify attempts)", linker.getCalls)
				}
			},
		},
		{
			// Verification fetch that errors on every retry is still a
			// soft-fail: the edit went through, so trust the body we wrote.
			name: "VerifyErrorTrustsBody",
			linker: &fakePRLinker{
				getQueue: []getResult{
					{issues: nil, body: "body"},
					{err: errors.New("network timeout")},
					{err: errors.New("network timeout")},
					{err: errors.New("network timeout")},
					{err: errors.New("network timeout")},
				},
			},
			ti:          baseTI(),
			taskStatus:  "in-review",
			checkStatus: "in-review",
			check: func(t *testing.T, out StepOutput, linker *fakePRLinker) {
				if !strings.Contains(out.Output, "trusting body") {
					t.Errorf("Output = %q, want 'trusting body' message", out.Output)
				}
			},
		},
		{
			name: "FetchErrorIsSoftFail",
			linker: &fakePRLinker{
				getQueue: []getResult{{err: errors.New("network timeout")}},
			},
			ti:          baseTI(),
			taskStatus:  "in-review",
			checkStatus: "in-review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			if !tc.noLinker {
				engine.SetPRLinker(tc.linker)
			}
			if tc.taskStatus != "" {
				tasks.Put(TaskInfo{ID: "t1", Status: tc.taskStatus})
			}

			out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), tc.ti)
			if err != nil {
				t.Fatal(err)
			}
			wantOutStatus := tc.wantOutStatus
			if wantOutStatus == "" {
				wantOutStatus = "completed"
			}
			if string(out.Status) != wantOutStatus {
				t.Errorf("Status = %q, want %q", out.Status, wantOutStatus)
			}
			if tc.check != nil {
				tc.check(t, out, tc.linker)
			}
			if tc.checkStatus != "" {
				after, _ := tasks.GetTask("t1")
				if after.Status != tc.checkStatus {
					t.Errorf("task status = %q, want %q", after.Status, tc.checkStatus)
				}
			}
		})
	}
}

func TestExecEnsurePRClosesIssue_MissingFieldsSkip(t *testing.T) {
	tests := []struct {
		name string
		ti   TaskInfo
	}{
		{"no issue", TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}},
		{"no pr", TaskInfo{ID: "t1", ProjectID: "owner/repo", Issue: "https://github.com/owner/repo/issues/7"}},
		{"no project", TaskInfo{ID: "t1", PRNumber: 5, Issue: "https://github.com/owner/repo/issues/7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			engine.SetPRLinker(&fakePRLinker{})

			out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), tt.ti)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" {
				t.Errorf("Status = %q, want completed", out.Status)
			}
			if !strings.Contains(out.Output, "skipped") {
				t.Errorf("Output = %q, want 'skipped' reason", out.Output)
			}
		})
	}
}

// TestExecStampPRAttribution tables the execStampPRAttribution outcomes
// that share the same store/tasks/agents/engine/linker setup and differ
// only in the linker's canned responses and what's asserted afterward.
func TestExecStampPRAttribution(t *testing.T) {
	baseTI := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}

	cases := []struct {
		name             string
		noLinker         bool
		linker           *fakePRLinker
		wantOutputSubstr string
		wantEditCalls    int // -1 = don't check
		check            func(t *testing.T, linker *fakePRLinker)
	}{
		{
			name:             "NoLinkerSkips",
			noLinker:         true,
			wantOutputSubstr: "no pr linker",
			wantEditCalls:    -1,
		},
		{
			name: "AppendsFooter",
			linker: &fakePRLinker{
				getQueue: []getResult{{body: "## Motivation\nfix it\n\nCloses https://github.com/owner/repo/issues/7"}},
			},
			wantOutputSubstr: "stamped",
			wantEditCalls:    1,
			check: func(t *testing.T, linker *fakePRLinker) {
				if !strings.HasSuffix(linker.lastBody, attribution.Footer) {
					t.Errorf("body = %q, want footer suffix", linker.lastBody)
				}
			},
		},
		{
			name:             "EmptyBodySkips",
			linker:           &fakePRLinker{getQueue: []getResult{{body: "   \n\t"}}},
			wantOutputSubstr: "empty pr body",
			wantEditCalls:    0,
		},
		{
			name: "IdempotentNoEdit",
			linker: &fakePRLinker{
				getQueue: []getResult{{body: "## Motivation\nfix it\n\n" + attribution.Footer}},
			},
			wantOutputSubstr: "already stamped",
			wantEditCalls:    0,
		},
		{
			name:             "FetchErrorIsSoftFail",
			linker:           &fakePRLinker{getQueue: []getResult{{err: errors.New("network timeout")}}},
			wantOutputSubstr: "",
			wantEditCalls:    0,
		},
		{
			name: "EditErrorIsSoftFail",
			linker: &fakePRLinker{
				getQueue: []getResult{{body: "body without footer"}},
				editErr:  errors.New("gh edit failed"),
			},
			wantOutputSubstr: "edit failed",
			wantEditCalls:    -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			if !tc.noLinker {
				engine.SetPRLinker(tc.linker)
			}

			out, err := engine.execStampPRAttribution("t1", newStampPRStep(), baseTI)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" {
				t.Errorf("Status = %q, want completed", out.Status)
			}
			if tc.wantOutputSubstr != "" && !strings.Contains(out.Output, tc.wantOutputSubstr) {
				t.Errorf("Output = %q, want %q", out.Output, tc.wantOutputSubstr)
			}
			if tc.wantEditCalls >= 0 && tc.linker.editCalls != tc.wantEditCalls {
				t.Errorf("editCalls = %d, want %d", tc.linker.editCalls, tc.wantEditCalls)
			}
			if tc.check != nil {
				tc.check(t, tc.linker)
			}
		})
	}
}

func TestExecStampPRAttribution_MissingFieldsSkip(t *testing.T) {
	tests := []struct {
		name string
		ti   TaskInfo
	}{
		{"no pr", TaskInfo{ID: "t1", ProjectID: "owner/repo"}},
		{"no project", TaskInfo{ID: "t1", PRNumber: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			linker := &fakePRLinker{}
			engine.SetPRLinker(linker)

			out, err := engine.execStampPRAttribution("t1", newStampPRStep(), tt.ti)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" || !strings.Contains(out.Output, "missing pr or project") {
				t.Errorf("out = %+v, want missing-fields skip", out)
			}
			if linker.getCalls != 0 || linker.editCalls != 0 {
				t.Errorf("linker touched: get=%d edit=%d, want 0/0", linker.getCalls, linker.editCalls)
			}
		})
	}
}

func TestExecEvaluate_LastAgentFailedQuarantines(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "rate limit exceeded"},
		},
	}

	out, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("step Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	if got := tasks.Reason("t1"); got != "rate limit exceeded" {
		t.Errorf("reason = %q, want %q", got, "rate limit exceeded")
	}
}

func TestExecEvaluate_LastAgentFailedKeepsFullReason(t *testing.T) {
	long := strings.Repeat("x", 500)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: long},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	got := tasks.Reason("t1")
	if strings.Contains(got, "(truncated)") {
		t.Errorf("reason should not be truncated: %q", got)
	}
	if got != long {
		t.Errorf("reason = %d chars, want the full %d-char output preserved", len(got), len(long))
	}
}

func TestExecEvaluate_LastAgentSucceededQuarantinesWithDefault(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "Implementation done."},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	if got := tasks.Reason("t1"); got != "commits pushed but no PR created" {
		t.Errorf("reason = %q, want %q", got, "commits pushed but no PR created")
	}
}

func TestExecEvaluate_PRCreateRateLimitParksForRetry(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		WorkflowID:  "simple-task-pr",
		CurrentStep: "evaluate",
		State:       ExecRunning,
		Variables:   map[string]string{},
		StepHistory: []StepRecord{
			{
				StepID:  "create_pr",
				Status:  "completed",
				AgentID: "a1",
				Output:  "GitHub GraphQL rate limit exhausted; I will wait for reset.",
			},
		},
	}

	_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "create_pr" {
		t.Errorf("CurrentStep = %q, want create_pr", wfExec.CurrentStep)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if _, ok := workflowRetryAfter(wfExec); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want ready-pr", ti.Status)
	}
	if got := tasks.Reason("t1"); got != prCreateRetryStatusReason {
		t.Errorf("reason = %q, want %q", got, prCreateRetryStatusReason)
	}
}

func TestExecEvaluate_PRCreateTransientOutageParksForRetry(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		WorkflowID:  "simple-task-pr",
		CurrentStep: "evaluate",
		State:       ExecRunning,
		Variables:   map[string]string{},
		StepHistory: []StepRecord{
			{
				StepID:  "create_pr",
				Status:  "completed",
				AgentID: "a1",
				Output:  "Network/auth is broken: connection refused to api.github.com. Please check connectivity.",
			},
		},
	}

	_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "create_pr" {
		t.Errorf("CurrentStep = %q, want create_pr", wfExec.CurrentStep)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if _, ok := workflowRetryAfter(wfExec); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want ready-pr", ti.Status)
	}
	if got := tasks.Reason("t1"); got != prCreateTransientStatusReason {
		t.Errorf("reason = %q, want %q", got, prCreateTransientStatusReason)
	}
}

func TestExecEvaluate_PRCreateAuthFailureRetriesThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	newWfExec := func(attempts string) *Execution {
		vars := map[string]string{}
		if attempts != "" {
			vars[prCreateAuthAttemptsVar] = attempts
		}
		return &Execution{
			WorkflowID:  "simple-task-pr",
			CurrentStep: "evaluate",
			State:       ExecRunning,
			Variables:   vars,
			StepHistory: []StepRecord{
				{
					StepID:  "create_pr",
					Status:  "completed",
					AgentID: "a1",
					Output:  "gh: Bad credentials (HTTP 401)",
				},
			},
		}
	}

	// First three attempts (0, 1, 2) park for retry and increment the counter.
	for i := range maxPRCreateAuthRetries {
		wfExec := newWfExec(strconv.Itoa(i))
		_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
		if wfExec.Variables[prCreateAuthAttemptsVar] != strconv.Itoa(i+1) {
			t.Errorf("attempt %d: %s = %q, want %q", i, prCreateAuthAttemptsVar, wfExec.Variables[prCreateAuthAttemptsVar], strconv.Itoa(i+1))
		}
		if got := tasks.Reason("t1"); got != prCreateAuthRetryReason {
			t.Errorf("attempt %d: reason = %q, want %q", i, got, prCreateAuthRetryReason)
		}
	}

	// After exhausting the budget, it records a machine quarantine instead of
	// retrying a broken credential forever.
	wfExec := newWfExec(strconv.Itoa(maxPRCreateAuthRetries))
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	wantReason := fmt.Sprintf("PR creation failing due to invalid or expired GitHub credentials after %d retries", maxPRCreateAuthRetries)
	if got := tasks.Reason("t1"); got != wantReason {
		t.Errorf("reason = %q, want %q", got, wantReason)
	}
}

func TestExecEvaluate_PRCreatePushedNoPRRetriesThenQuarantines(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := newEngineForEval(t, tasks)
	newWfExec := func(attempts string) *Execution {
		vars := map[string]string{}
		if attempts != "" {
			vars[prCreateAttemptsVar] = attempts
		}
		return &Execution{
			WorkflowID:  "simple-task-pr",
			CurrentStep: "evaluate",
			State:       ExecRunning,
			Variables:   vars,
			StepHistory: []StepRecord{
				{
					StepID:  "create_pr",
					Status:  "completed",
					AgentID: "a1",
					Output:  "I was unable to create the PR due to an unexpected issue.",
				},
			},
		}
	}

	// First three attempts (0, 1, 2) park for retry and increment the counter.
	for i := range maxPRCreatePushedNoPRRetries {
		wfExec := newWfExec(strconv.Itoa(i))
		_, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"})
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
		if wfExec.Variables[prCreateAttemptsVar] != strconv.Itoa(i+1) {
			t.Errorf("attempt %d: %s = %q, want %q", i, prCreateAttemptsVar, wfExec.Variables[prCreateAttemptsVar], strconv.Itoa(i+1))
		}
		if got := tasks.Reason("t1"); got != prCreatePushedNoPRReason {
			t.Errorf("attempt %d: reason = %q, want %q", i, got, prCreatePushedNoPRReason)
		}
	}

	// After exhausting the budget, it records a machine quarantine.
	wfExec := newWfExec(strconv.Itoa(maxPRCreatePushedNoPRRetries))
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr"}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	wantReason := fmt.Sprintf("commits pushed but no PR created after %d retries", maxPRCreatePushedNoPRRetries)
	if got := tasks.Reason("t1"); got != wantReason {
		t.Errorf("reason = %q, want %q", got, wantReason)
	}
}

func TestExecEvaluate_SkipsMechanicalStepsInHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "real error"},
			{StepID: "verify_commits", Status: "completed"},
			{StepID: "link_pr_and_review", Status: "completed", Output: "no pr found"},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "real error" {
		t.Errorf("reason = %q, want %q (mechanical steps must be skipped)", got, "real error")
	}
}

func TestExecEvaluate_EmptyHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "blocked" {
		t.Errorf("task status = %q, want blocked", ti.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
	if got := tasks.Reason("t1"); got != "no agent result to evaluate" {
		t.Errorf("reason = %q, want %q", got, "no agent result to evaluate")
	}
}

func TestExecEvaluate_FailedWithEmptyOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "   "},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "agent failed with no output" {
		t.Errorf("reason = %q, want %q", got, "agent failed with no output")
	}
}

func TestExecEvaluate_NoPRFallsThrough(t *testing.T) {
	// When ProjectID+Branch are set but gh pr list finds nothing, the step must
	// still fall through to a machine quarantine (not panic or error).
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "owner/repo", Branch: "feature-branch"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "timed out"},
		},
	}

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", Branch: "feature-branch"}
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, ti); err != nil {
		t.Fatal(err)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "blocked" {
		t.Errorf("status = %q, want blocked", got.Status)
	}
	assertWorkflowMachineQuarantine(t, tasks, "t1", "workflow.evaluate_no_pr")
}

func TestExecLinkPRAndReview_PRAlreadyLinked(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 42})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 42 {
		t.Errorf("pr_number = %d, want 42", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_PRNumberNotInRepoFallsThrough(t *testing.T) {
	// A pr_number that doesn't resolve against the project's own repo (e.g.
	// an agent that ran a bare `gh pr create` inside a fork worktree and got
	// a PR opened in the fork itself) must not be trusted blindly — it
	// should fall through to the other discovery paths instead of flipping
	// straight to in-review against a PR nobody upstream will ever see.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.setPRExistenceCheckerForTest(fakePRExistenceChecker{exists: false})
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "changes pushed"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not trust the wrong-repo pr_number)", ti.Status)
	}
}

func TestExecLinkPRAndReview_PRNumberUnverifiedFallsThrough(t *testing.T) {
	// A checker that fails to confirm (gh unavailable/unauthenticated,
	// network) must be treated the same as "not confirmed" — never as proof
	// the PR is absent, but also never trusted outright.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.setPRExistenceCheckerForTest(fakePRExistenceChecker{err: errors.New("gh: authentication failed")})

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not trust an unverifiable pr_number)", ti.Status)
	}
}

func TestExecLinkPRAndReview_PRNumberVerifiedInRepoTrusted(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)
	engine.setPRExistenceCheckerForTest(fakePRExistenceChecker{exists: true})

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 8 {
		t.Errorf("pr_number = %d, want 8", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_NoCheckerTrustsPRNumber(t *testing.T) {
	// Guards the documented "operates with a nil checker" fallback contract.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 8, ProjectID: "kumahq/kuma"})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 8, ProjectID: "kumahq/kuma"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
}

func TestExecLinkPRAndReview_FullURLInAgentOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{
				StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: https://github.com/owner/repo/pull/123",
			},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 123 {
		t.Errorf("pr_number = %d, want 123", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_ShortRefInAgentOutput(t *testing.T) {
	// Agents sometimes output "owner/repo#N" instead of a full GitHub URL.
	// The step must parse this shorthand and link the PR.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{
				StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: Automaat/sybra#444\n\nChanges applied.",
			},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 444 {
		t.Errorf("pr_number = %d, want 444", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_NoPRFallsThrough(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "changes pushed"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not change)", ti.Status)
	}
}
