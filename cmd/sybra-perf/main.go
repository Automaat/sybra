// sybra-perf is a token-free end-to-end load harness for Sybra.
//
// It boots a throwaway sybra-server subprocess in an isolated SYBRA_HOME,
// points the server at the fake-claude test double (via PATH injection) so
// no real LLM API tokens are spent, drives the server over the HTTP dispatch
// API, and collects latency + throughput statistics. Optionally pulls
// Prometheus metrics and pprof profiles for deeper investigation.
//
// Scenarios:
//
//	steady — ramp to N concurrent agents and hold for the scenario duration.
//	         Each agent runs the fake-claude perf_stream scenario, producing
//	         a controlled NDJSON stream. Measures agent startup latency and
//	         steady-state event throughput.
//
//	spike  — start concurrency agents as fast as possible, no hold.
//	         Each agent runs fake-claude perf_burst. Stresses the 50ms
//	         emit throttle and concurrency cap bookkeeping.
//
//	soak   — start concurrency agents using fake-claude perf_long for the
//	         full scenario duration. Used with pprof heap diffs to surface
//	         goroutine or buffer leaks.
//
//	churn  — no agents; hammer TaskService.CreateTask / UpdateTask / DeleteTask
//	         at the configured rate. Measures task-store latency and watcher
//	         debounce behavior under bursty writes.
//
// Output: JSON report on stdout. Non-zero exit on setup failure or if the
// scenario's error budget is exceeded.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	scenario      string
	concurrency   int
	duration      time.Duration
	rampDuration  time.Duration
	eventCount    int
	eventInterval time.Duration
	churnRate     int
	fakeClaude    string
	workDir       string
	pprofDir      string
	reportPath    string
	keepHome      bool
	verbose       bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.scenario, "scenario", "steady", "steady | spike | soak | churn")
	flag.IntVar(&o.concurrency, "concurrency", 4, "target concurrent agents (steady/spike/soak) or parallel workers (churn)")
	flag.DurationVar(&o.duration, "duration", 30*time.Second, "total scenario runtime")
	flag.DurationVar(&o.rampDuration, "ramp", 2*time.Second, "ramp-up duration for steady/soak")
	flag.IntVar(&o.eventCount, "events", 100, "fake-claude event count for perf_stream / perf_burst")
	flag.DurationVar(&o.eventInterval, "event-interval", 10*time.Millisecond, "fake-claude inter-event interval for perf_stream / perf_long")
	flag.IntVar(&o.churnRate, "churn-rate", 100, "task operations per second for churn scenario")
	flag.StringVar(&o.fakeClaude, "fake-claude", "", "path to fake-claude binary (default: go run ./cmd/fake-claude)")
	flag.StringVar(&o.workDir, "workdir", "", "SYBRA_HOME to use (default: a tmp dir)")
	flag.StringVar(&o.pprofDir, "pprof-dir", "", "if set, write heap/goroutine profiles here before and after the run")
	flag.StringVar(&o.reportPath, "report", "", "if set, write JSON report to this path instead of stdout")
	flag.BoolVar(&o.keepHome, "keep-home", false, "do not delete the temp SYBRA_HOME after the run")
	flag.BoolVar(&o.verbose, "verbose", false, "log server output and per-agent progress")
	flag.Parse()
	return o
}

func main() {
	o := parseFlags()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "sybra-perf:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if err := validateScenario(o.scenario); err != nil {
		return err
	}

	home, cleanupHome, err := prepareHome(o)
	if err != nil {
		return fmt.Errorf("prepare home: %w", err)
	}
	if !o.keepHome {
		defer cleanupHome()
	}

	fakeDir, err := resolveFakeClaude(o.fakeClaude)
	if err != nil {
		return fmt.Errorf("resolve fake-claude: %w", err)
	}

	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick port: %w", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	srv, err := startServer(o, home, fakeDir, port)
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer srv.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := waitHealthy(ctx, baseURL, 30*time.Second); err != nil {
		return fmt.Errorf("server never became healthy: %w", err)
	}

	client := newAPIClient(baseURL)

	if o.pprofDir != "" {
		if err := os.MkdirAll(o.pprofDir, 0o755); err != nil {
			return fmt.Errorf("pprof dir: %w", err)
		}
		_ = fetchProfile(ctx, baseURL, "heap", filepath.Join(o.pprofDir, "heap-before.pb.gz"))
		_ = fetchProfile(ctx, baseURL, "goroutine", filepath.Join(o.pprofDir, "goroutine-before.pb.gz"))
	}

	report := &Report{
		Scenario:    o.scenario,
		Concurrency: o.concurrency,
		Duration:    o.duration.String(),
		StartedAt:   time.Now().UTC(),
	}

	// Capture a baseline of key Prometheus counters before the scenario runs
	// so the report can show the delta (how much the scenario actually
	// moved the needle) rather than cumulative server lifetime totals.
	if m, err := scrapeMetrics(ctx, baseURL+"/metrics"); err == nil {
		report.MetricsBefore = m
	}

	switch o.scenario {
	case "steady":
		err = runSteady(ctx, client, o, report)
	case "spike":
		err = runSpike(ctx, client, o, report)
	case "soak":
		err = runSoak(ctx, client, o, report)
	case "churn":
		err = runChurn(ctx, client, o, report)
	}
	if err != nil {
		report.FatalError = err.Error()
	}

	if m, err := scrapeMetrics(ctx, baseURL+"/metrics"); err == nil {
		report.MetricsAfter = m
	}

	if o.pprofDir != "" {
		_ = fetchProfile(ctx, baseURL, "heap", filepath.Join(o.pprofDir, "heap-after.pb.gz"))
		_ = fetchProfile(ctx, baseURL, "goroutine", filepath.Join(o.pprofDir, "goroutine-after.pb.gz"))
	}

	report.FinishedAt = time.Now().UTC()
	report.finalize()

	if err := writeReport(report, o.reportPath); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if report.FatalError != "" {
		return errors.New(report.FatalError)
	}
	return nil
}

func validateScenario(s string) error {
	switch s {
	case "steady", "spike", "soak", "churn":
		return nil
	default:
		return fmt.Errorf("unknown scenario %q (valid: steady, spike, soak, churn)", s)
	}
}

// prepareHome sets up an isolated SYBRA_HOME with a perf-tuned config:
// metrics on, pollers off, generous concurrency cap, research_machine_dir
// populated so research-type tasks skip worktree setup. Returns the home
// path and a cleanup function.
func prepareHome(o options) (home string, cleanup func(), err error) {
	if o.workDir != "" {
		abs, absErr := filepath.Abs(o.workDir)
		if absErr != nil {
			return "", nil, absErr
		}
		home = abs
		if mkErr := os.MkdirAll(home, 0o755); mkErr != nil {
			return "", nil, mkErr
		}
	} else {
		dir, tmpErr := os.MkdirTemp("", "sybra-perf-*")
		if tmpErr != nil {
			return "", nil, tmpErr
		}
		home = dir
	}

	// Dirs the server would otherwise create under HomeDir(); creating them
	// up-front lets us point research_machine_dir at an existing path. Build
	// each directory absolutely so a typo in one entry can't silently place
	// the research dir in the wrong place.
	researchDir := filepath.Join(home, "research")
	for _, sub := range []string{"tasks", "logs", "clones", "worktrees", "projects", "loop-agents"} {
		if mkErr := os.MkdirAll(filepath.Join(home, sub), 0o755); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			return home, nil, mkErr
		}
	}
	if mkErr := os.MkdirAll(researchDir, 0o755); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
		return home, nil, mkErr
	}

	// Write a minimal config that:
	// - enables metrics (for /metrics scraping)
	// - disables all pollers (todoist, github, renovate) so the server has
	//   no background noise competing with the scenario
	// - lifts the concurrency cap to at least 2× the requested concurrency
	// - opts agents out of require_permissions so fake-claude runs with
	//   --dangerously-skip-permissions (avoids any approval flow)
	// - points research_machine_dir at the pre-created researchDir so
	//   research-type tasks skip worktree setup
	maxConcurrent := max(o.concurrency*2, 8)
	cfg := fmt.Sprintf(`# sybra-perf temp config
metrics:
  enabled: true
agent:
  max_concurrent: %d
  max_cost_usd: 0
  max_turns: 0
  research_machine_dir: %s
  require_permissions: false
todoist:
  enabled: false
github:
  enabled: false
renovate:
  enabled: false
notification:
  desktop: false
providers:
  health_check:
    enabled: false
  auto_failover: false
`, maxConcurrent, researchDir)
	if wrErr := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); wrErr != nil {
		return home, nil, wrErr
	}

	cleanup = func() {
		_ = os.RemoveAll(home)
	}
	return home, cleanup, nil
}

// resolveFakeClaude returns a directory that contains a binary named "claude"
// which is the fake-claude test double. If an explicit path is supplied, we
// symlink it into a private dir so PATH-injection is clean. Otherwise we
// build fake-claude via `go build` into a temp dir.
func resolveFakeClaude(explicit string) (string, error) {
	dir, err := os.MkdirTemp("", "sybra-perf-fake-*")
	if err != nil {
		return "", err
	}

	var src string
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		src = abs
	} else {
		src = filepath.Join(dir, "fake-claude")
		cmd := exec.Command("go", "build", "-o", src, "./cmd/fake-claude")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("go build fake-claude: %w", err)
		}
	}

	// Runner looks up "claude" by name via exec.LookPath, so symlink the
	// fake binary under that name inside our private dir. Fall back to copy
	// if symlink fails (e.g. tmpfs with no symlink support).
	claudePath := filepath.Join(dir, "claude")
	if err := os.Symlink(src, claudePath); err != nil {
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			return "", fmt.Errorf("read fake-claude: %w", readErr)
		}
		if err := os.WriteFile(claudePath, data, 0o755); err != nil {
			return "", fmt.Errorf("write fake-claude copy: %w", err)
		}
	}
	return dir, nil
}

type serverProc struct {
	cmd *exec.Cmd
}

func (s *serverProc) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

func startServer(o options, home, fakeDir string, port int) (*serverProc, error) {
	// Always exec via `go run ./cmd/sybra-server` — a literal-string
	// invocation keeps gosec G204 silent and avoids the footgun of a stale
	// pre-built binary masking recent server changes. Go build caching
	// makes repeat runs of the same scenario pay the compile cost only once.
	cmd := exec.Command("go", "run", "./cmd/sybra-server")

	perfPath := fakeDir + string(os.PathListSeparator) + os.Getenv("PATH")
	env := os.Environ()
	env = filterEnv(env, []string{"SYBRA_HOME", "SYBRA_PORT", "SYBRA_PPROF", "SYBRA_TASKS_DIR", "SYBRA_DISABLE_WORKFLOWS", "PATH", "FAKE_CLAUDE_SCENARIO", "FAKE_CLAUDE_EVENT_COUNT", "FAKE_CLAUDE_EVENT_INTERVAL_MS", "FAKE_CLAUDE_DURATION_MS"})
	env = append(env,
		"SYBRA_HOME="+home,
		"SYBRA_PORT="+strconv.Itoa(port),
		"SYBRA_PPROF=1",
		"SYBRA_DISABLE_WORKFLOWS=1",
		"PATH="+perfPath,
		"FAKE_CLAUDE_SCENARIO="+scenarioToFake(o.scenario),
		"FAKE_CLAUDE_EVENT_COUNT="+strconv.Itoa(o.eventCount),
		"FAKE_CLAUDE_EVENT_INTERVAL_MS="+strconv.Itoa(int(o.eventInterval/time.Millisecond)),
		"FAKE_CLAUDE_DURATION_MS="+strconv.Itoa(int(o.duration/time.Millisecond)),
	)
	cmd.Env = env

	if o.verbose {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &serverProc{cmd: cmd}, nil
}

func scenarioToFake(scenario string) string {
	switch scenario {
	case "spike":
		return "perf_burst"
	case "soak":
		return "perf_long"
	case "churn":
		return "success" // churn scenario does not start agents
	default:
		return "perf_stream"
	}
}

// filterEnv strips env vars that we intend to overwrite, to avoid PATH
// duplication and stale FAKE_CLAUDE_* values from the parent shell.
func filterEnv(env, drop []string) []string {
	dropSet := make(map[string]struct{}, len(drop))
	for _, k := range drop {
		dropSet[k] = struct{}{}
	}
	out := env[:0]
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if _, skip := dropSet[key]; skip {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("pickFreePort: listener is not a *net.TCPAddr")
	}
	return addr.Port, nil
}

// waitHealthy blocks until GET /health returns 200 or ctx/deadline expires.
// Polls every 100ms. Required because startServer returns immediately after
// the subprocess is spawned; the server listener is not yet bound.
func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s", timeout)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", http.NoBody)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ==================== API client ====================

type apiClient struct {
	baseURL string
	http    *http.Client
}

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// call invokes POST /api/{service}/{method} with the given positional args.
// Args are JSON-encoded as an array per httpapi.Mount's contract. out may be
// nil when the caller does not care about the response body.
func (c *apiClient) call(service, method string, args []any, out any) error {
	var body []byte
	var err error
	if len(args) > 0 {
		body, err = json.Marshal(args)
		if err != nil {
			return err
		}
	}
	url := fmt.Sprintf("%s/api/%s/%s", c.baseURL, service, method)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s.%s: %s (%d)", service, method, strings.TrimSpace(string(msg)), resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ==================== Task helpers ====================

type taskPayload struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// createResearchTask creates a task then flips its task_type to "research"
// so StartAgent takes the skipWorktree path and runs against the configured
// research_machine_dir. Avoids the cost of real git-worktree setup.
func (c *apiClient) createResearchTask(i int) (string, error) {
	var t taskPayload
	if err := c.call("TaskService", "CreateTask", []any{
		fmt.Sprintf("perf task %d", i),
		"## Description\nperf harness body\n",
		"headless",
	}, &t); err != nil {
		return "", fmt.Errorf("CreateTask: %w", err)
	}
	if t.ID == "" {
		return "", errors.New("CreateTask returned empty id")
	}
	if err := c.call("TaskService", "UpdateTask", []any{t.ID, map[string]any{"task_type": "research"}}, nil); err != nil {
		return "", fmt.Errorf("UpdateTask task_type=research: %w", err)
	}
	return t.ID, nil
}

// startAgent invokes App.StartAgent and returns the spawned agent's in-memory
// ID so the caller can correlate SSE output/state events back to this run.
func (c *apiClient) startAgent(taskID string) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.call("App", "StartAgent", []any{taskID, "headless", "perf harness prompt"}, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (c *apiClient) deleteTask(id string) error {
	return c.call("TaskService", "DeleteTask", []any{id}, nil)
}

// ==================== Scenarios ====================

// agentResult carries per-agent measurements. A zero startLatency means the
// agent never produced a first event before the scenario ended.
type agentResult struct {
	taskID       string
	agentID      string
	startedAt    time.Time
	startLatency time.Duration
	totalEvents  int
	err          string
}

func runSteady(ctx context.Context, c *apiClient, o options, report *Report) error {
	return runConcurrentAgents(ctx, c, o, report, "steady")
}

func runSpike(ctx context.Context, c *apiClient, o options, report *Report) error {
	// Spike differs from steady only in ramp=0 and the perf_burst fake
	// scenario (already wired in startServer).
	o.rampDuration = 0
	return runConcurrentAgents(ctx, c, o, report, "spike")
}

func runSoak(ctx context.Context, c *apiClient, o options, report *Report) error {
	return runConcurrentAgents(ctx, c, o, report, "soak")
}

// eventCollector drains an SSE channel and exposes per-agent counters
// through sync.Maps. It owns the goroutine that walks the channel and
// returns totals when drained.
type eventCollector struct {
	counts  sync.Map // agentID → int
	done    sync.Map // agentID → time.Time of result event
	tasks   sync.Map // taskID → latest agentID seen on state events
	total   atomic.Int64
	drained chan struct{}
}

func (ec *eventCollector) start(events <-chan sseEvent) {
	ec.drained = make(chan struct{})
	go func() {
		defer close(ec.drained)
		for ev := range events {
			ec.total.Add(1)
			const statePrefix = "agent:state:"
			if strings.HasPrefix(ev.name, statePrefix) {
				var payload struct {
					ID     string `json:"id"`
					TaskID string `json:"taskId"`
				}
				if err := json.Unmarshal([]byte(ev.data), &payload); err == nil && payload.TaskID != "" && payload.ID != "" {
					ec.tasks.Store(payload.TaskID, payload.ID)
				}
			}
			// Only agent:output:{id} events carry per-agent throughput info.
			// agent:state:{id} fires once per state transition and is
			// counted in total but not broken down per agent here.
			const prefix = "agent:output:"
			if !strings.HasPrefix(ev.name, prefix) {
				continue
			}
			agentID := ev.name[len(prefix):]
			cur, _ := ec.counts.LoadOrStore(agentID, 0)
			if v, ok := cur.(int); ok {
				ec.counts.Store(agentID, v+1)
			}
			if strings.Contains(ev.data, `"type":"result"`) {
				ec.done.Store(agentID, time.Now())
			}
		}
	}()
}

func (ec *eventCollector) wait() {
	if ec.drained == nil {
		return
	}
	<-ec.drained
}

func (ec *eventCollector) sum() int {
	var total int
	ec.counts.Range(func(_, v any) bool {
		if n, ok := v.(int); ok {
			total += n
		}
		return true
	})
	return total
}

func (ec *eventCollector) forAgent(agentID string) int {
	if agentID == "" {
		return 0
	}
	v, ok := ec.counts.Load(agentID)
	if !ok {
		return 0
	}
	n, _ := v.(int)
	return n
}

func (ec *eventCollector) agentForTask(taskID, fallback string) string {
	if taskID == "" {
		return fallback
	}
	v, ok := ec.tasks.Load(taskID)
	if !ok {
		return fallback
	}
	id, _ := v.(string)
	if id == "" {
		return fallback
	}
	return id
}

// launchAgents fans out concurrency goroutines that each create a task and
// call StartAgent, spacing their starts linearly over rampDuration. Returns
// once every goroutine has recorded its agentResult.
func launchAgents(ctx context.Context, c *apiClient, o options) []agentResult {
	results := make([]agentResult, o.concurrency)
	var wg sync.WaitGroup
	for i := range o.concurrency {
		var delay time.Duration
		if o.concurrency > 1 && o.rampDuration > 0 {
			delay = time.Duration(i) * o.rampDuration / time.Duration(o.concurrency-1)
		}
		wg.Go(func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			r := runOneAgent(ctx, c, i)
			results[i] = r
			if o.verbose {
				log.Printf("agent[%d] task=%s err=%q events=%d", i, r.taskID, r.err, r.totalEvents)
			}
		})
	}
	wg.Wait()
	return results
}

// holdForDuration blocks until the configured scenario duration has elapsed
// (counted from start), or until ctx is cancelled. Adds a short drain window
// so in-flight SSE events emitted by late-finishing agents are still captured.
func holdForDuration(ctx context.Context, start time.Time, duration time.Duration) {
	remaining := duration - time.Since(start)
	if remaining > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(remaining):
		}
	}
	time.Sleep(250 * time.Millisecond)
}

// runConcurrentAgents drives `concurrency` agents concurrently and collects
// their results. Agents are started linearly over rampDuration, then held
// for duration minus the ramp. Each agent runs with the fake-claude scenario
// set by startServer; this function is transport-agnostic.
func runConcurrentAgents(ctx context.Context, c *apiClient, o options, report *Report, kind string) error {
	scenarioCtx, cancel := context.WithTimeout(ctx, o.duration+30*time.Second)
	defer cancel()

	resp, events, err := subscribeEvents(scenarioCtx, c.baseURL+"/events")
	if err != nil {
		return fmt.Errorf("subscribe /events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var ec eventCollector
	ec.start(events)

	start := time.Now()
	results := launchAgents(scenarioCtx, c, o)
	holdForDuration(scenarioCtx, start, o.duration)
	_ = resp.Body.Close()
	ec.wait()

	report.Kind = kind
	report.TotalAgents = o.concurrency
	report.TotalEvents = int(ec.total.Load())
	report.OutputEvents = ec.sum()
	report.Elapsed = time.Since(start).String()
	report.Agents = make([]agentReport, 0, len(results))
	for _, r := range results {
		agentID := ec.agentForTask(r.taskID, r.agentID)
		report.Agents = append(report.Agents, agentReport{
			TaskID:             r.taskID,
			AgentID:            agentID,
			StartLatencyMS:     r.startLatency.Milliseconds(),
			Error:              r.err,
			ObservedEventCount: ec.forAgent(agentID),
		})
	}
	return nil
}

// runOneAgent creates a research task, starts a headless agent against it,
// and measures only the HTTP startup latency. Per-agent event counts are
// collected separately via the SSE listener in the caller.
func runOneAgent(_ context.Context, c *apiClient, i int) agentResult {
	r := agentResult{startedAt: time.Now()}
	id, err := c.createResearchTask(i)
	if err != nil {
		r.err = err.Error()
		return r
	}
	r.taskID = id
	callStart := time.Now()
	agentID, err := c.startAgent(id)
	if err != nil {
		r.err = err.Error()
		return r
	}
	r.agentID = agentID
	r.startLatency = time.Since(callStart)
	// runOneAgent returns as soon as StartAgent is accepted — the caller's
	// SSE listener records events emitted by the agent afterward.
	return r
}

func runChurn(ctx context.Context, c *apiClient, o options, report *Report) error {
	if o.churnRate <= 0 {
		return errors.New("churn-rate must be > 0")
	}
	deadline := time.Now().Add(o.duration)
	ticker := time.NewTicker(time.Second / time.Duration(o.churnRate))
	defer ticker.Stop()

	var (
		createLat []time.Duration
		updateLat []time.Duration
		deleteLat []time.Duration
		errCount  atomic.Int64
		total     atomic.Int64
		mu        sync.Mutex
	)

	sem := make(chan struct{}, o.concurrency)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			goto done
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			goto done
		}
		select {
		case sem <- struct{}{}:
		default:
			// worker pool saturated — count as backpressure event
			continue
		}
		seq := total.Load()
		wg.Go(func() {
			defer func() { <-sem }()

			cStart := time.Now()
			var t taskPayload
			if err := c.call("TaskService", "CreateTask", []any{
				fmt.Sprintf("churn task %d", seq),
				"## body\nchurn body\n",
				"headless",
			}, &t); err != nil {
				errCount.Add(1)
				return
			}
			cDur := time.Since(cStart)

			uStart := time.Now()
			// Update only the body field: tags / status transitions would
			// trigger hooks (orchestrator, notifier, workflow engine) and
			// contaminate pure CRUD latency. Body edits are side-effect-free.
			if err := c.call("TaskService", "UpdateTask", []any{t.ID, map[string]any{
				"body": fmt.Sprintf("## Description\nupdated body %d\n", seq),
			}}, nil); err != nil {
				errCount.Add(1)
				return
			}
			uDur := time.Since(uStart)

			dStart := time.Now()
			if err := c.deleteTask(t.ID); err != nil {
				errCount.Add(1)
				return
			}
			dDur := time.Since(dStart)

			mu.Lock()
			createLat = append(createLat, cDur)
			updateLat = append(updateLat, uDur)
			deleteLat = append(deleteLat, dDur)
			mu.Unlock()
			total.Add(1)
		})
	}

done:
	wg.Wait()
	report.Kind = "churn"
	report.ChurnTotal = int(total.Load())
	report.ChurnErrors = int(errCount.Load())
	report.CreateLatency = summarize(createLat)
	report.UpdateLatency = summarize(updateLat)
	report.DeleteLatency = summarize(deleteLat)
	return nil
}

// ==================== SSE ====================

type sseEvent struct {
	name string
	data string
}

// subscribeEvents opens an SSE connection to /events and returns both the
// raw response and a channel of parsed events. The caller owns the response
// body and must defer resp.Body.Close(). Surfacing *http.Response across the
// function boundary is what lets bodyclose statically verify the lifecycle
// — it refuses to trace through a goroutine or a struct-held io.Closer, so
// the body has to sit in a named return value the caller can see.
// Closing the response body unblocks the scanner and closes the events chan.
func subscribeEvents(ctx context.Context, url string) (*http.Response, <-chan sseEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("sse subscribe: status %d", resp.StatusCode)
	}

	out := make(chan sseEvent, 1024)
	body := resp.Body
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		var curEvent, curData string
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if curEvent != "" || curData != "" {
					select {
					case out <- sseEvent{name: curEvent, data: curData}:
					case <-ctx.Done():
						return
					}
				}
				curEvent, curData = "", ""
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				curEvent = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				curData = line[len("data: "):]
			}
		}
	}()

	return resp, out, nil
}

// ==================== Metrics / pprof ====================

// httpGet wraps http.Client.Do so gosec G107 sees a single, shared request
// construction site. The baseURL is produced by sybra-perf itself (loopback
// + ephemeral port picked via pickFreePort), not from untrusted input.
func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// scrapeMetrics downloads /metrics and extracts a handful of Prometheus
// counters we care about (sybra_agent_*, process_resident_memory_bytes,
// go_goroutines). Stores raw values in a map keyed by metric name. Missing
// metrics are silently skipped so older server builds still produce a report.
func scrapeMetrics(ctx context.Context, url string) (map[string]float64, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: %d", url, resp.StatusCode)
	}
	out := map[string]float64{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	prefixes := []string{
		"sybra_agent_",
		"sybra_task_",
		"process_resident_memory_bytes",
		"process_cpu_seconds_total",
		"go_goroutines",
		"go_memstats_alloc_bytes",
		"go_memstats_heap_inuse_bytes",
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		var matched bool
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// "<name>{labels} <value>" — take the last whitespace-separated token
		// as the value and the rest as the key.
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key := line[:sp]
		valStr := line[sp+1:]
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out[key] = v
	}
	return out, nil
}

// fetchProfile downloads a net/http/pprof profile and writes it to disk.
// "name" is one of heap, goroutine, profile, etc.
func fetchProfile(ctx context.Context, baseURL, name, dest string) error {
	url := fmt.Sprintf("%s/debug/pprof/%s", baseURL, name)
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pprof %s: %d", name, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, resp.Body)
	return err
}

// ==================== Report ====================

type Report struct {
	Scenario     string        `json:"scenario"`
	Kind         string        `json:"kind,omitempty"`
	Concurrency  int           `json:"concurrency"`
	Duration     string        `json:"duration"`
	StartedAt    time.Time     `json:"startedAt"`
	FinishedAt   time.Time     `json:"finishedAt"`
	Elapsed      string        `json:"elapsed,omitempty"`
	TotalAgents  int           `json:"totalAgents,omitempty"`
	TotalEvents  int           `json:"totalEvents,omitempty"`
	OutputEvents int           `json:"outputEvents,omitempty"`
	Agents       []agentReport `json:"agents,omitempty"`

	ChurnTotal     int           `json:"churnTotal,omitempty"`
	ChurnErrors    int           `json:"churnErrors,omitempty"`
	CreateLatency  *latencyStats `json:"createLatencyMS,omitempty"`
	UpdateLatency  *latencyStats `json:"updateLatencyMS,omitempty"`
	DeleteLatency  *latencyStats `json:"deleteLatencyMS,omitempty"`
	StartupLatency *latencyStats `json:"startupLatencyMS,omitempty"`

	MetricsBefore map[string]float64 `json:"metricsBefore,omitempty"`
	MetricsAfter  map[string]float64 `json:"metricsAfter,omitempty"`

	Host       hostInfo `json:"host"`
	FatalError string   `json:"fatalError,omitempty"`
}

type agentReport struct {
	TaskID             string `json:"taskId"`
	AgentID            string `json:"agentId,omitempty"`
	StartLatencyMS     int64  `json:"startLatencyMS"`
	Error              string `json:"error,omitempty"`
	ObservedEventCount int    `json:"observedEventCount"`
}

type latencyStats struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

type hostInfo struct {
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
	NumCPU  int    `json:"numCPU"`
	Version string `json:"goVersion"`
}

func (r *Report) finalize() {
	r.Host = hostInfo{
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		NumCPU:  runtime.NumCPU(),
		Version: runtime.Version(),
	}
	if r.Elapsed == "" {
		r.Elapsed = r.FinishedAt.Sub(r.StartedAt).String()
	}
	// Populate startup latency from per-agent records so consumers get
	// percentile stats without walking the Agents array themselves.
	if len(r.Agents) > 0 {
		lats := make([]time.Duration, 0, len(r.Agents))
		for _, a := range r.Agents {
			if a.StartLatencyMS > 0 {
				lats = append(lats, time.Duration(a.StartLatencyMS)*time.Millisecond)
			}
		}
		if s := summarize(lats); s != nil {
			r.StartupLatency = s
		}
	}
}

func summarize(samples []time.Duration) *latencyStats {
	if len(samples) == 0 {
		return nil
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	slices.Sort(sorted)
	toMS := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	pct := func(p float64) float64 {
		idx := int(float64(len(sorted)-1) * p)
		return toMS(sorted[idx])
	}
	var sum time.Duration
	for _, s := range sorted {
		sum += s
	}
	return &latencyStats{
		Count: len(sorted),
		Min:   toMS(sorted[0]),
		P50:   pct(0.50),
		P95:   pct(0.95),
		P99:   pct(0.99),
		Max:   toMS(sorted[len(sorted)-1]),
		Mean:  toMS(sum / time.Duration(len(sorted))),
	}
}

func writeReport(r *Report, path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if path == "" {
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
