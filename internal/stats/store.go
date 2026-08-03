package stats

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/skillattr"
)

const storeLockTimeout = 2 * time.Second

// Store persists RunRecords to a JSON file and computes aggregates in memory.
type Store struct {
	path         string
	mu           sync.Mutex
	runs         []RunRecord
	legacyFormat bool
	offset       int64
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	unlock, err := fsutil.LockFileWithin(path, storeLockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	if s.legacyFormat {
		if err := s.rewriteLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Record appends r as one NDJSON line while holding the cross-process lock.
// This avoids repeatedly decoding and atomically rewriting the full history
// on every agent completion. The lock is acquired before s.mu so a stalled
// peer process cannot block in-process stats readers.
func (s *Store) Record(r RunRecord) error {
	unlock, err := fsutil.LockFileWithin(s.path, storeLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.syncLocked(); err != nil {
		return err
	}
	if err := s.appendLocked([]RunRecord{r}); err != nil {
		return err
	}
	s.runs = append(s.runs, r)
	return nil
}

// reloadLocked re-reads both legacy JSON arrays and the current NDJSON format
// into s.runs. Callers must hold s.mu; callers that may race another process
// must also hold the cross-process file lock.
func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.runs = nil
			s.offset = 0
			return nil
		}
		return err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		s.runs = nil
		s.legacyFormat = false
		s.offset = int64(len(data))
		return nil
	}
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &s.runs); err != nil {
			return err
		}
		s.legacyFormat = true
		s.offset = int64(len(data))
		return nil
	}
	data, err = repairIncompleteTail(s.path, data)
	if err != nil {
		return err
	}
	runs, err := parseNDJSON(data, 1)
	if err != nil {
		return err
	}
	s.runs = runs
	s.legacyFormat = false
	s.offset = int64(len(data))
	return nil
}

// syncLocked incrementally loads records appended by another process since
// offset. It must hold both the file lock and s.mu.
func (s *Store) syncLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) && s.offset == 0 {
			return nil
		}
		return err
	}
	if int64(len(data)) < s.offset {
		return s.reloadLocked()
	}
	data, err = repairIncompleteTail(s.path, data)
	if err != nil {
		return err
	}
	if int64(len(data)) < s.offset {
		return s.reloadLocked()
	}
	if int64(len(data)) == s.offset {
		return nil
	}
	firstLine := bytes.Count(data[:s.offset], []byte{'\n'}) + 1
	runs, err := parseNDJSON(data[s.offset:], firstLine)
	if err != nil {
		return err
	}
	s.runs = append(s.runs, runs...)
	s.offset = int64(len(data))
	return nil
}

// syncForRead makes already-open stores observe another process's append.
// It avoids taking the file lock when the file has not grown, keeping normal
// UI and evaluation reads independent from a peer that merely holds the lock.
func (s *Store) syncForRead() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.Lock()
	offset := s.offset
	s.mu.Unlock()
	if info.Size() <= offset {
		return
	}
	unlock, err := fsutil.LockFileWithin(s.path, storeLockTimeout)
	if err != nil {
		return
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.syncLocked()
}

func parseNDJSON(data []byte, firstLine int) ([]RunRecord, error) {
	lines := bytes.Split(data, []byte{'\n'})
	runs := make([]RunRecord, 0, len(lines))
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var r RunRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("parse stats record on line %d: %w", firstLine+i, err)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// repairIncompleteTail removes only an unterminated final NDJSON record. A
// crashed writer can leave that suffix behind; newline-terminated malformed
// records remain an error so genuine data corruption is not hidden.
func repairIncompleteTail(path string, data []byte) ([]byte, error) {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data, nil
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if err := os.Truncate(path, int64(end)); err != nil {
		return nil, fmt.Errorf("truncate incomplete stats record: %w", err)
	}
	return data[:end], nil
}

func (s *Store) Len() int {
	s.syncForRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// All returns a copy of every recorded run. Used by the evaluation service to
// compute scorecards over an arbitrary time window (Query only exposes fixed
// windows and the last 50 runs).
func (s *Store) All() []RunRecord {
	s.syncForRead()
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.runs)
}

func (s *Store) Query() StatsResponse {
	return s.QueryAt(time.Now())
}

func (s *Store) QueryAt(now time.Time) StatsResponse {
	s.syncForRead()
	s.mu.Lock()
	defer s.mu.Unlock()

	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -int(todayStart.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// Preallocate to len(s.runs): `all` is exactly that size, the time-window
	// subsets are bounded by it. Measured at 5k records: -23% wall, -17%
	// bytes, -35% allocs vs uncapped append. Query runs on every stats UI
	// open.
	n := len(s.runs)
	all := make([]RunRecord, 0, n)
	today := make([]RunRecord, 0, n)
	week := make([]RunRecord, 0, n)
	month := make([]RunRecord, 0, n)
	byProject := map[string][]RunRecord{}
	byMode := map[string][]RunRecord{}
	byRole := map[string][]RunRecord{}
	byModel := map[string][]RunRecord{}
	byProvider := map[string][]RunRecord{}
	bySkillExecutionMode := map[string][]RunRecord{}

	for i := range s.runs {
		r := normalizedRunRecord(s.runs[i])
		all = append(all, r)

		if !r.Timestamp.Before(todayStart) {
			today = append(today, r)
		}
		if !r.Timestamp.Before(weekStart) {
			week = append(week, r)
		}
		if !r.Timestamp.Before(monthStart) {
			month = append(month, r)
		}

		pid := r.ProjectID
		if pid == "" {
			pid = "(none)"
		}
		byProject[pid] = append(byProject[pid], r)

		byMode[r.Mode] = append(byMode[r.Mode], r)

		role := r.Role
		if role == "" {
			role = "implementation"
		}
		byRole[role] = append(byRole[role], r)

		model := r.Model
		if model == "" {
			model = "(unknown)"
		}
		byModel[model] = append(byModel[model], r)

		provider := r.Provider
		if provider == "" {
			provider = "(unknown)"
		}
		byProvider[provider] = append(byProvider[provider], r)

		bySkillExecutionMode[r.SkillExecutionMode] = append(bySkillExecutionMode[r.SkillExecutionMode], r)
	}

	resp := StatsResponse{
		Today:                summarize(today),
		ThisWeek:             summarize(week),
		ThisMonth:            summarize(month),
		AllTime:              summarize(all),
		ByProject:            groupedStats(byProject),
		ByMode:               groupedStats(byMode),
		ByRole:               groupedStats(byRole),
		ByModel:              groupedStats(byModel),
		ByProvider:           groupedStats(byProvider),
		BySkillExecutionMode: groupedStats(bySkillExecutionMode),
		ReviewRounds:         reviewRoundsByModel(s.runs),
	}

	// Recent runs: last 50, newest first
	recent := make([]RunRecord, len(s.runs))
	for i := range s.runs {
		recent[i] = normalizedRunRecord(s.runs[i])
	}
	slices.SortFunc(recent, func(a, b RunRecord) int { return b.Timestamp.Compare(a.Timestamp) })
	if len(recent) > 50 {
		recent = recent[:50]
	}
	resp.RecentRuns = recent

	return resp
}

func normalizedRunRecord(r RunRecord) RunRecord {
	r.SkillExecutionMode = skillattr.NormalizeExecutionMode(r.SkillExecutionMode)
	r.SkillConformance = skillattr.NormalizeConformance(r.SkillConformance)
	return r
}

// appendLocked appends records in one write so a locked peer never observes a
// partially written record. Callers must hold both the file lock and s.mu.
func (s *Store) appendLocked(records []RunRecord) error {
	if len(records) == 0 {
		return nil
	}
	data, err := marshalNDJSON(records)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	start := s.offset
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = fmt.Errorf("short stats append: wrote %d of %d bytes", n, len(data))
	}
	if err != nil {
		_ = f.Truncate(start)
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.offset += int64(len(data))
	return nil
}

// rewriteLocked is used only to migrate the legacy JSON array format. Normal
// writes never rewrite the accumulated history.
func (s *Store) rewriteLocked() error {
	data, err := marshalNDJSON(s.runs)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(s.path, data); err != nil {
		return err
	}
	s.legacyFormat = false
	s.offset = int64(len(data))
	return nil
}

func marshalNDJSON(records []RunRecord) ([]byte, error) {
	var data bytes.Buffer
	for i := range records {
		line, err := json.Marshal(records[i])
		if err != nil {
			return nil, err
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	return data.Bytes(), nil
}

// roleReviewLabel mirrors agent.RoleReview and bestOfNAssignmentUnit mirrors
// the assignment workflow stamps on best-of-N attempts. Duplicated rather than
// imported: internal/agent already imports internal/stats for
// EstimateAgentCost, so a back-import would form a cycle. Outcome values are
// NOT duplicated — this package owns them (OutcomeCompleted/Failed/Stalled),
// and task.RunOutcomeSuccess ("success") is a different vocabulary that never
// reaches RunRecord.Outcome (see completion.runOutcome).
const (
	roleReviewLabel       = "review"
	bestOfNAssignmentUnit = "bestofn-attempt"
)

// taskReviewRollup accumulates one task's implementation attribution and its
// review-round count while walking the run log a single time.
type taskReviewRollup struct {
	implModel  string
	implSeen   bool
	mixedImpl  bool
	rounds     int
	firstImplT time.Time
	bestOfN    bool
}

// normalizeRunModel resolves the model label for grouping. RunRecord.Model is
// empty for older records and for providers that never reported one.
func normalizeRunModel(m string) string {
	if m == "" {
		return "(unknown)"
	}
	return m
}

// isImplementationRun reports whether a run authored the implementation.
// AgentRun.Role carries "" for implementation on older records (see
// task.AgentRun), which is why an empty role counts here — the same
// normalization ByRole applies.
func isImplementationRun(role string) bool {
	return role == "" || role == "implementation"
}

// reviewRoundsByModel groups tasks by the model that implemented them and
// reports how many review rounds each needed.
//
// Three exclusions keep the number a measure of code quality rather than of
// harness noise:
//
//   - Tasks with no review run: they never entered the review loop (skipped as
//     trivial/noreview, or still in flight), so counting them as zero rounds
//     would present code that was never reviewed as code a reviewer passed on
//     the first try.
//   - Runs that did not complete: recordRunStats writes a record for every
//     agent termination, so a reviewer that crashed or stalled and was retried
//     would otherwise read as a second round — conflating provider flakiness
//     with rework, the exact confound this stat exists to isolate. Only
//     OutcomeCompleted counts; OutcomeFailed and OutcomeStalled do not.
//   - Best-of-N tasks entirely: simple-task-best-of-n-implement dispatches its
//     judge with role "review", and nothing on the RunRecord separates that
//     judge from a real reviewer. Counting it would add a phantom round to
//     every best-of-N task and deny it CleanFirstPass — biased against the
//     model that won the bake-off. Dropping the task is honest; guessing which
//     review run was the judge is not.
func reviewRoundsByModel(runs []RunRecord) []ReviewRoundsStat {
	rollups := map[string]*taskReviewRollup{}

	for i := range runs {
		r := &runs[i]
		if r.TaskID == "" {
			continue
		}
		tr := rollups[r.TaskID]
		if tr == nil {
			tr = &taskReviewRollup{}
			rollups[r.TaskID] = tr
		}
		if r.AssignmentUnit == bestOfNAssignmentUnit {
			tr.bestOfN = true
		}
		if r.Outcome != OutcomeCompleted {
			continue
		}
		switch {
		case isImplementationRun(r.Role):
			model := normalizeRunModel(r.Model)
			// Ordering within the run log is not guaranteed, so attribution
			// follows the earliest implementation timestamp, not position.
			if !tr.implSeen || r.Timestamp.Before(tr.firstImplT) {
				if tr.implSeen && model != tr.implModel {
					tr.mixedImpl = true
				}
				tr.implModel = model
				tr.firstImplT = r.Timestamp
				tr.implSeen = true
			} else if model != tr.implModel {
				tr.mixedImpl = true
			}
		case r.Role == roleReviewLabel:
			tr.rounds++
		}
	}

	agg := map[string]*ReviewRoundsStat{}
	for _, tr := range rollups {
		if tr.rounds == 0 || !tr.implSeen || tr.bestOfN {
			continue
		}
		st := agg[tr.implModel]
		if st == nil {
			st = &ReviewRoundsStat{Key: tr.implModel}
			agg[tr.implModel] = st
		}
		st.Tasks++
		st.TotalRounds += tr.rounds
		if tr.rounds > st.MaxRounds {
			st.MaxRounds = tr.rounds
		}
		if tr.rounds == 1 {
			st.CleanFirstPass++
		}
		if tr.mixedImpl {
			st.MixedImplModels++
		}
	}

	out := make([]ReviewRoundsStat, 0, len(agg))
	for _, st := range agg {
		st.AvgRounds = float64(st.TotalRounds) / float64(st.Tasks)
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b ReviewRoundsStat) int {
		if c := cmp.Compare(b.Tasks, a.Tasks); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}

func summarize(runs []RunRecord) Summary {
	if len(runs) == 0 {
		return Summary{}
	}
	s := Summary{OutcomeCounts: map[string]int{}}
	s.TotalRuns = len(runs)
	for i := range runs {
		s.TotalCostUSD += runs[i].CostUSD
		s.TotalDurationS += runs[i].DurationS
		s.TotalInputTokens += runs[i].InputTokens
		s.TotalOutputTokens += runs[i].OutputTokens
		s.TotalCacheCreationInputTokens += runs[i].CacheCreationInputTokens
		s.TotalCacheReadInputTokens += runs[i].CacheReadInputTokens
		s.TotalReasoningTokens += runs[i].ReasoningTokens
		s.TotalPremiumRequests += runs[i].PremiumRequests
		outcome := runs[i].Outcome
		if outcome == "" {
			outcome = "unknown"
		}
		s.OutcomeCounts[outcome]++
		if outcome == "failed" {
			s.FailedRuns++
		}
	}
	s.AvgCostPerRun = s.TotalCostUSD / float64(s.TotalRuns)
	s.AvgDurationS = s.TotalDurationS / float64(s.TotalRuns)
	return s
}

func groupedStats(groups map[string][]RunRecord) []GroupedStat {
	result := make([]GroupedStat, 0, len(groups))
	for key, runs := range groups {
		result = append(result, GroupedStat{Key: key, Stats: summarize(runs)})
	}
	slices.SortFunc(result, func(a, b GroupedStat) int { return cmp.Compare(b.Stats.TotalCostUSD, a.Stats.TotalCostUSD) })
	return result
}
