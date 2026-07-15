package stats

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/skillattr"
)

// Store persists RunRecords to a JSON file and computes aggregates in memory.
type Store struct {
	path string
	mu   sync.Mutex
	runs []RunRecord
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Record appends r and persists it. s.runs is populated once at NewStore
// time and never re-read afterward on the query paths, so a naive
// append-and-flush here would silently drop any run written by another
// process (sybra-cli and the GUI server each hold their own Store over the
// same path) in the gap since this process last loaded the file. Reloading
// from disk under the cross-process flock, immediately before appending,
// closes that gap: the in-memory s.runs is resynced to the authoritative
// on-disk state before this run is added.
func (s *Store) Record(r RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	s.runs = append(s.runs, r)
	return s.flush()
}

// reloadLocked re-reads s.path into s.runs. Read-modify-write callers
// (Record) must hold both s.mu and the cross-process file lock across the
// whole critical section; NewStore calls it unlocked because it runs before
// s is returned to any caller, so nothing else can observe or race it yet.
func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.runs = nil
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.runs = nil
		return nil
	}
	var runs []RunRecord
	if err := json.Unmarshal(data, &runs); err != nil {
		return err
	}
	s.runs = runs
	return nil
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// All returns a copy of every recorded run. Used by the evaluation service to
// compute scorecards over an arbitrary time window (Query only exposes fixed
// windows and the last 50 runs).
func (s *Store) All() []RunRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.runs)
}

func (s *Store) Query() StatsResponse {
	return s.QueryAt(time.Now())
}

func (s *Store) QueryAt(now time.Time) StatsResponse {
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

func (s *Store) flush() error {
	data, err := json.Marshal(s.runs)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, data)
}

func summarize(runs []RunRecord) Summary {
	if len(runs) == 0 {
		return Summary{}
	}
	var s Summary
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
