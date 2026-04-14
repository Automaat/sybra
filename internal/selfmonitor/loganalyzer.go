// Package selfmonitor contains the background service that runs the periodic
// deep-analysis loop over Synapse agent runs: distills NDJSON agent logs into
// a structured LogSummary, judges findings with a two-stage LLM pipeline, and
// autonomously remediates a whitelisted set of categories.
//
// Phase A is silent — the primitives (loganalyzer, ledger, fingerprints,
// report types) ship without any wiring so callers can adopt them without
// runtime behavior change.
package selfmonitor

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Automaat/synapse/internal/agent"
)

// LogSummarySchemaVersion is bumped whenever the shape of LogSummary changes
// in a way that downstream prompts or UI consumers need to know about.
const LogSummarySchemaVersion = 1

// DefaultMaxEvents caps how many of the most recent NDJSON events Analyze
// keeps in memory. 2000 covers all but the most pathological long-running
// agents without blowing up.
const DefaultMaxEvents = 2000

// DefaultLastToolCalls is how many trailing tool calls Analyze records
// verbatim in LogSummary.LastToolCalls for the LLM's final look.
const DefaultLastToolCalls = 10

// RepeatedCallThreshold is the minimum number of structurally-identical tool
// invocations before a RepeatedCall entry is emitted.
const RepeatedCallThreshold = 3

// StallWindow is the number of trailing tool calls that must share the same
// input hash for StallDetected to fire.
const StallWindow = 5

// LogSummary is the distilled, structured projection of an agent NDJSON log
// that downstream consumers (the two-stage LLM judge, the GUI, correlators)
// read instead of the raw file. Produced by Analyze.
type LogSummary struct {
	SchemaVersion       int                `json:"schemaVersion"`
	Path                string             `json:"path,omitempty"`
	TotalEvents         int                `json:"totalEvents"`
	TotalToolCalls      int                `json:"totalToolCalls"`
	TotalCostUSD        float64            `json:"totalCostUsd"`
	TotalInputTokens    int                `json:"totalInputTokens"`
	TotalOutputTokens   int                `json:"totalOutputTokens"`
	ToolHistogram       map[string]int     `json:"toolHistogram"`
	RepeatedCalls       []RepeatedCall     `json:"repeatedCalls,omitempty"`
	InferredCostPerTool map[string]float64 `json:"inferredCostPerTool"`
	ErrorClasses        []ErrorClass       `json:"errorClasses,omitempty"`
	LastToolCalls       []ToolCallTrace    `json:"lastToolCalls,omitempty"`
	StallDetected       bool               `json:"stallDetected"`
	StallReason         string             `json:"stallReason,omitempty"`
	FinalError          string             `json:"finalError,omitempty"`
}

// RepeatedCall reports a tool invocation that fired ≥ RepeatedCallThreshold
// times with structurally identical Input — the classic tool-loop signature.
type RepeatedCall struct {
	Tool        string         `json:"tool"`
	Count       int            `json:"count"`
	InputHash   string         `json:"inputHash"`
	SampleInput map[string]any `json:"sampleInput,omitempty"`
}

// ToolCallTrace is a trailing tool call recorded verbatim so the judge LLM
// can see the tail of the run without reading the raw log.
type ToolCallTrace struct {
	Tool          string         `json:"tool"`
	Input         map[string]any `json:"input,omitempty"`
	ResultExcerpt string         `json:"resultExcerpt,omitempty"`
	IsError       bool           `json:"isError,omitempty"`
}

// ErrorClass aggregates tool_result / result-event errors by a coarse
// category (rate_limit, permission_denied, ...).
type ErrorClass struct {
	Class  string `json:"class"`
	Count  int    `json:"count"`
	Sample string `json:"sample,omitempty"`
}

// Analyze reads an NDJSON agent log and produces a LogSummary. It is safe to
// call on files written by either the Claude or Codex runner — both formats
// are detected line-by-line via the agent package's parsers.
//
// maxEvents bounds memory usage: when set, only the last N parsed events are
// retained before aggregation. Pass 0 to use DefaultMaxEvents.
func Analyze(path string, maxEvents int) (LogSummary, error) {
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	events, err := parseRich(path, maxEvents)
	if err != nil {
		return LogSummary{}, err
	}
	s := aggregate(events)
	s.Path = path
	return s, nil
}

// parseRich reads an NDJSON log file and returns a slice of agent.ClaudeEvent
// with tool-use / tool-result / result-cost fields populated. Lines that fail
// Claude parsing fall back to Codex parsing; CodexEvent shares the same
// *ClaudeMessage / *ClaudeResult payload so the result is a unified stream.
func parseRich(path string, maxEvents int) ([]agent.ClaudeEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []agent.ClaudeEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, perr := agent.ParseClaudeLine(line)
		if perr != nil || ev.Type == "" {
			ce, cerr := agent.ParseCodexLine(line)
			if cerr != nil || ce.Type == "" {
				continue
			}
			ev = agent.ClaudeEvent{
				Type:      ce.Type,
				Subtype:   ce.Subtype,
				SessionID: ce.SessionID,
				Message:   ce.Message,
				Result:    ce.Result,
			}
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	return events, nil
}

// pendingCall tracks a tool invocation awaiting a result event so cost can
// be attributed back to the tool that earned it. Lifted to package scope so
// the distributeCost helper can take it as a typed slice.
type pendingCall struct {
	useID    string
	name     string
	bytesIn  int
	bytesOut int
}

// liveCall is the in-flight record attached to a single tool_use ID so the
// subsequent tool_result block can stamp its excerpt and error flag onto the
// trace the analyzer emits to the LLM.
type liveCall struct {
	trace     ToolCallTrace
	inputHash string
}

// analyzerState holds the mutable working set of the reducer. Lifting it out
// of aggregate keeps the top-level function short enough for the funlen lint.
type analyzerState struct {
	summary           LogSummary
	perToolHashes     map[string]map[string]int
	perToolSample     map[string]map[string]map[string]any
	since             []pendingCall
	sinceByUseID      map[string]int
	byUseID           map[string]*liveCall
	order             []string
	errorClassCounts  map[string]int
	errorClassSamples map[string]string
}

func newAnalyzerState(total int) *analyzerState {
	return &analyzerState{
		summary: LogSummary{
			SchemaVersion:       LogSummarySchemaVersion,
			TotalEvents:         total,
			ToolHistogram:       map[string]int{},
			InferredCostPerTool: map[string]float64{},
		},
		perToolHashes:     map[string]map[string]int{},
		perToolSample:     map[string]map[string]map[string]any{},
		sinceByUseID:      map[string]int{},
		byUseID:           map[string]*liveCall{},
		errorClassCounts:  map[string]int{},
		errorClassSamples: map[string]string{},
	}
}

// aggregate is the pure reducer over a parsed event stream. Kept separate
// from Analyze so tests can construct fixture event slices directly without
// touching the filesystem.
func aggregate(events []agent.ClaudeEvent) LogSummary {
	st := newAnalyzerState(len(events))
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case "assistant":
			st.onAssistant(&ev)
		case "user":
			st.onUser(&ev)
		case "result":
			st.onResult(&ev)
		}
	}
	st.finalize()
	return st.summary
}

func (st *analyzerState) onAssistant(ev *agent.ClaudeEvent) {
	if ev.Message == nil {
		return
	}
	for _, t := range ev.Message.ToolUses {
		st.summary.TotalToolCalls++
		st.summary.ToolHistogram[t.Name]++
		hash, sample := canonicalInputHash(t.Input)
		if st.perToolHashes[t.Name] == nil {
			st.perToolHashes[t.Name] = map[string]int{}
			st.perToolSample[t.Name] = map[string]map[string]any{}
		}
		st.perToolHashes[t.Name][hash]++
		if _, ok := st.perToolSample[t.Name][hash]; !ok {
			st.perToolSample[t.Name][hash] = sample
		}
		st.since = append(st.since, pendingCall{useID: t.ID, name: t.Name, bytesIn: estimateInputBytes(t.Input)})
		st.sinceByUseID[t.ID] = len(st.since) - 1
		st.byUseID[t.ID] = &liveCall{trace: ToolCallTrace{Tool: t.Name, Input: sample}, inputHash: hash}
		st.order = append(st.order, t.ID)
	}
}

func (st *analyzerState) onUser(ev *agent.ClaudeEvent) {
	if ev.Message == nil {
		return
	}
	for _, r := range ev.Message.ToolResults {
		excerpt := truncate(r.Content, 400)
		if call, ok := st.byUseID[r.ToolUseID]; ok {
			call.trace.ResultExcerpt = excerpt
			call.trace.IsError = r.IsError
		}
		// Attribute output bytes to the matching tool_use by ID. Falls back
		// to recency for malformed streams lacking tool_use_id.
		if idx, ok := st.sinceByUseID[r.ToolUseID]; ok && idx >= 0 && idx < len(st.since) {
			st.since[idx].bytesOut += len(r.Content)
		} else if len(st.since) > 0 {
			st.since[len(st.since)-1].bytesOut += len(r.Content)
		}
		if r.IsError {
			class := classifyToolResultError(r.Content)
			st.errorClassCounts[class]++
			if _, ok := st.errorClassSamples[class]; !ok {
				st.errorClassSamples[class] = excerpt
			}
		}
	}
}

func (st *analyzerState) onResult(ev *agent.ClaudeEvent) {
	if ev.Result == nil {
		return
	}
	st.summary.TotalCostUSD += ev.Result.CostUSD
	st.summary.TotalInputTokens += ev.Result.InputTokens
	st.summary.TotalOutputTokens += ev.Result.OutputTokens
	distributeCost(st.summary.InferredCostPerTool, st.since, ev.Result.CostUSD)
	st.since = st.since[:0]
	clear(st.sinceByUseID)
	if ev.Subtype != "error" && ev.Result.ErrorType == "" {
		return
	}
	class := classifyResultError(ev.Result.ErrorType, ev.Result.ErrorStatus, ev.Result.Text)
	st.errorClassCounts[class]++
	if _, ok := st.errorClassSamples[class]; !ok {
		st.errorClassSamples[class] = truncate(ev.Result.Text, 400)
	}
	if class != "" {
		st.summary.FinalError = class
	}
}

func (st *analyzerState) finalize() {
	st.populateRepeatedCalls()
	st.populateErrorClasses()
	st.populateLastToolCalls()
	st.detectStall()
}

func (st *analyzerState) populateRepeatedCalls() {
	for tool, hashes := range st.perToolHashes {
		for hash, count := range hashes {
			if count < RepeatedCallThreshold {
				continue
			}
			st.summary.RepeatedCalls = append(st.summary.RepeatedCalls, RepeatedCall{
				Tool:        tool,
				Count:       count,
				InputHash:   hash,
				SampleInput: st.perToolSample[tool][hash],
			})
		}
	}
	sort.Slice(st.summary.RepeatedCalls, func(i, j int) bool {
		if st.summary.RepeatedCalls[i].Count != st.summary.RepeatedCalls[j].Count {
			return st.summary.RepeatedCalls[i].Count > st.summary.RepeatedCalls[j].Count
		}
		return st.summary.RepeatedCalls[i].Tool < st.summary.RepeatedCalls[j].Tool
	})
}

func (st *analyzerState) populateErrorClasses() {
	for class, count := range st.errorClassCounts {
		st.summary.ErrorClasses = append(st.summary.ErrorClasses, ErrorClass{
			Class:  class,
			Count:  count,
			Sample: st.errorClassSamples[class],
		})
	}
	sort.Slice(st.summary.ErrorClasses, func(i, j int) bool {
		if st.summary.ErrorClasses[i].Count != st.summary.ErrorClasses[j].Count {
			return st.summary.ErrorClasses[i].Count > st.summary.ErrorClasses[j].Count
		}
		return st.summary.ErrorClasses[i].Class < st.summary.ErrorClasses[j].Class
	})
}

func (st *analyzerState) populateLastToolCalls() {
	start := max(len(st.order)-DefaultLastToolCalls, 0)
	for _, id := range st.order[start:] {
		if call, ok := st.byUseID[id]; ok {
			st.summary.LastToolCalls = append(st.summary.LastToolCalls, call.trace)
		}
	}
}

func (st *analyzerState) detectStall() {
	if len(st.order) < StallWindow {
		return
	}
	tail := st.order[len(st.order)-StallWindow:]
	first, ok := st.byUseID[tail[0]]
	if !ok {
		return
	}
	sameTool := first.trace.Tool
	for _, id := range tail[1:] {
		c, ok := st.byUseID[id]
		if !ok || c.inputHash != first.inputHash || c.trace.Tool != sameTool {
			return
		}
	}
	st.summary.StallDetected = true
	st.summary.StallReason = "last " + itoa(StallWindow) + " tool calls are identical " + sameTool + " invocations"
}

// canonicalInputHash returns a stable SHA-256 hex digest of a tool-use input
// map plus a defensive copy of the map safe to embed in the summary. Maps
// are marshaled with json.Marshal which sorts keys for deterministic output.
func canonicalInputHash(input map[string]any) (hash string, sample map[string]any) {
	if len(input) == 0 {
		return "empty", map[string]any{}
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "unmarshalable", cloneMap(input)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12]), cloneMap(input)
}

func cloneMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	maps.Copy(out, m)
	return out
}

func estimateInputBytes(input map[string]any) int {
	if len(input) == 0 {
		return 0
	}
	data, err := json.Marshal(input)
	if err != nil {
		return 0
	}
	return len(data)
}

// distributeCost splits a result-event cost across the tool calls since the
// previous result, proportional to (bytes_in + bytes_out). Tools with zero
// bytes split the cost evenly so the attribution is never lost.
func distributeCost(dst map[string]float64, since []pendingCall, cost float64) {
	if cost <= 0 || len(since) == 0 {
		return
	}
	var total int
	for _, p := range since {
		total += p.bytesIn + p.bytesOut
	}
	if total == 0 {
		share := cost / float64(len(since))
		for _, p := range since {
			dst[p.name] += share
		}
		return
	}
	for _, p := range since {
		w := float64(p.bytesIn+p.bytesOut) / float64(total)
		dst[p.name] += cost * w
	}
}

// classifyResultError maps a structured Claude/Codex result-event error into
// a coarse class usable for cross-run aggregation.
func classifyResultError(errorType string, status int, text string) string {
	et := strings.ToLower(errorType)
	switch {
	case strings.Contains(et, "rate_limit") || status == 429:
		return "rate_limit"
	case strings.Contains(et, "overloaded"):
		return "overloaded_error"
	case strings.Contains(et, "authentication") || status == 401 || status == 403:
		return "auth_error"
	case strings.Contains(et, "network") || strings.Contains(et, "connection"):
		return "network"
	case et != "":
		return et
	}
	return classifyByText(text)
}

var (
	rePermissionDenied = regexp.MustCompile(`(?i)(permission denied|EACCES|operation not permitted)`)
	reRateLimit        = regexp.MustCompile(`(?i)(rate limit|too many requests|429)`)
	reNetwork          = regexp.MustCompile(`(?i)(connection refused|network|dial tcp|i/o timeout|dns)`)
	reNotFound         = regexp.MustCompile(`(?i)(not found|no such file|404)`)
)

// classifyToolResultError maps a tool_result block's error content into a
// coarse class. Used for aggregating tool-level failures (permission denied,
// not found, ...) across a run.
func classifyToolResultError(content string) string {
	switch {
	case rePermissionDenied.MatchString(content):
		return "permission_denied"
	case reRateLimit.MatchString(content):
		return "rate_limit"
	case reNetwork.MatchString(content):
		return "network"
	case reNotFound.MatchString(content):
		return "not_found"
	}
	return "tool_error"
}

func classifyByText(text string) string {
	if text == "" {
		return "unknown"
	}
	switch {
	case rePermissionDenied.MatchString(text):
		return "permission_denied"
	case reRateLimit.MatchString(text):
		return "rate_limit"
	case reNetwork.MatchString(text):
		return "network"
	}
	return "unknown"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
