package audit

import "github.com/Automaat/sybra/internal/version"

type factoryInterval struct {
	start, end Event
	censored   bool
}

func factoryIntervals(events []Event, phase, release string) FactoryPhase {
	intervals := map[string]*factoryInterval{}
	var samples factorySamples
	var completed []factoryInterval
	for _, e := range events {
		key, state := factoryBoundary(e, phase)
		if state == "" {
			continue
		}
		if key == "" {
			if releaseMatches(release, factoryRelease(e.Release)) {
				samples.unknown++
			}
			continue
		}
		pair := intervals[key]
		if (phase == "ci" || phase == "deploy") && state == "start" && pair != nil && pair.end.Type != "" {
			// A second rollout of the same SHA after rollback is a new episode,
			// just like rerunning checks on the same head after verification.
			completed = append(completed, *pair)
			pair = nil
		}
		if pair == nil {
			pair = &factoryInterval{}
			intervals[key] = pair
		}
		switch state {
		case "start":
			if pair.start.Type == "" {
				pair.start = e
			}
			// Restored queue intent remains the same interval. A newer queued
			// observation makes it open again until its eventual dequeue.
			if phase == "queue" {
				pair.end = Event{}
				pair.censored = false
			}
		case "end":
			if pair.end.Type == "" || phase == "queue" {
				pair.end = e
				pair.censored = false
			}
		case "censored":
			pair.end = e
			pair.censored = true
		}
	}
	for i := range completed {
		pair := &completed[i]
		label := pairRelease(pair.start, pair.end)
		if phase == "deploy" {
			key, _ := factoryBoundary(pair.start, phase)
			if key == "" {
				key, _ = factoryBoundary(pair.end, phase)
			}
			label = factoryRelease(key)
		}
		if releaseMatches(release, label) {
			samples.add(pair.start.Timestamp, pair.end.Timestamp, pair.censored)
		}
	}
	for key, pair := range intervals {
		label := pairRelease(pair.start, pair.end)
		if phase == "deploy" {
			label = factoryRelease(key)
		} // target release, not outgoing leader
		if releaseMatches(release, label) {
			samples.add(pair.start.Timestamp, pair.end.Timestamp, pair.censored)
		}
	}
	return samples.report()
}

func factoryBoundary(e Event, phase string) (key, state string) {
	key, _ = e.Data["interval_key"].(string)
	switch phase {
	case "queue":
		if e.Type == EventFactoryQueue {
			switch e.Data["state"] {
			case "queued":
				state = "start"
			case "dequeued":
				state = "end"
			case "removed":
				state = "censored"
			}
		}
	case "ci":
		switch e.Type {
		case EventFactoryCIWait:
			state = "start"
		case EventFactoryCIVerified:
			state = "end"
		}
	case "deploy":
		if e.Type == EventAutoUpdateTransition {
			key, _ = e.Data["candidate"].(string)
			switch e.Data["transition"] {
			case "seen":
				state = "start"
			case "superseded":
				state = "censored"
			}
		}
		if e.Type == EventFactoryReleaseStarted {
			key, state = e.Release, "end"
		}
		if !version.ValidRevision(key) {
			key = ""
		}
	}
	return key, state
}
