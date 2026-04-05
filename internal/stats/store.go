package stats

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/fsutil"
)

// Store persists RunRecords to a JSON file and computes aggregates in memory.
type Store struct {
	path string
	mu   sync.Mutex
	runs []RunRecord
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}

	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.runs); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Record(r RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.runs = append(s.runs, r)
	return s.flush()
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *Store) Query() StatsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -int(todayStart.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var today, week, month, all []RunRecord
	byProject := map[string][]RunRecord{}
	byMode := map[string][]RunRecord{}
	byRole := map[string][]RunRecord{}
	byModel := map[string][]RunRecord{}

	for i := range s.runs {
		r := &s.runs[i]
		all = append(all, *r)

		if !r.Timestamp.Before(todayStart) {
			today = append(today, *r)
		}
		if !r.Timestamp.Before(weekStart) {
			week = append(week, *r)
		}
		if !r.Timestamp.Before(monthStart) {
			month = append(month, *r)
		}

		pid := r.ProjectID
		if pid == "" {
			pid = "(none)"
		}
		byProject[pid] = append(byProject[pid], *r)

		byMode[r.Mode] = append(byMode[r.Mode], *r)

		role := r.Role
		if role == "" {
			role = "implementation"
		}
		byRole[role] = append(byRole[role], *r)

		model := r.Model
		if model == "" {
			model = "(unknown)"
		}
		byModel[model] = append(byModel[model], *r)
	}

	resp := StatsResponse{
		Today:     summarize(today),
		ThisWeek:  summarize(week),
		ThisMonth: summarize(month),
		AllTime:   summarize(all),
		ByProject: groupedStats(byProject),
		ByMode:    groupedStats(byMode),
		ByRole:    groupedStats(byRole),
		ByModel:   groupedStats(byModel),
	}

	// Recent runs: last 50, newest first
	recent := make([]RunRecord, len(s.runs))
	copy(recent, s.runs)
	sort.Slice(recent, func(i, j int) bool {
		return recent[i].Timestamp.After(recent[j].Timestamp)
	})
	if len(recent) > 50 {
		recent = recent[:50]
	}
	resp.RecentRuns = recent

	return resp
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
	sort.Slice(result, func(i, j int) bool {
		return result[i].Stats.TotalCostUSD > result[j].Stats.TotalCostUSD
	})
	return result
}
