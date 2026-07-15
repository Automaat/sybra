package harnessevolution

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

func ClusterEvents(events []FailureEvent, minSize int) []Cluster {
	if minSize <= 0 {
		minSize = 2
	}
	byKey := make(map[string][]FailureEvent)
	for i := range events {
		ev := events[i]
		key := clusterKey(ev)
		byKey[key] = append(byKey[key], ev)
	}

	clusters := make([]Cluster, 0, len(byKey))
	for key, grouped := range byKey {
		if len(grouped) < minSize {
			continue
		}
		slices.SortFunc(grouped, func(a, b FailureEvent) int {
			return a.OccurredAt.Compare(b.OccurredAt)
		})
		clusters = append(clusters, Cluster{
			Key:          key,
			Cause:        clusterCause(grouped[0]),
			Count:        len(grouped),
			Events:       slices.Clone(grouped),
			FirstSeen:    firstTime(grouped),
			LastSeen:     lastTime(grouped),
			AffectedStep: grouped[0].WorkflowStep,
			Category:     grouped[0].Category,
			FailureKind:  grouped[0].FailureKind,
		})
	}
	slices.SortFunc(clusters, func(a, b Cluster) int {
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return clusters
}

func clusterKey(ev FailureEvent) string {
	parts := []string{
		normalizeToken(ev.Category),
		normalizeToken(ev.FailureKind),
		normalizeToken(ev.WorkflowStep),
		normalizeToken(ev.Role),
		normalizeToken(ev.Provider),
	}
	raw := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:12]
}

func clusterCause(ev FailureEvent) string {
	parts := []string{ev.Category}
	if ev.FailureKind != "" && ev.FailureKind != ev.Category {
		parts = append(parts, ev.FailureKind)
	}
	if ev.WorkflowStep != "" {
		parts = append(parts, "step:"+ev.WorkflowStep)
	}
	if ev.Role != "" {
		parts = append(parts, "role:"+ev.Role)
	}
	return strings.Join(parts, " / ")
}

func firstTime(events []FailureEvent) time.Time {
	if len(events) == 0 {
		return time.Time{}
	}
	return events[0].OccurredAt
}

func lastTime(events []FailureEvent) time.Time {
	if len(events) == 0 {
		return time.Time{}
	}
	return events[len(events)-1].OccurredAt
}
