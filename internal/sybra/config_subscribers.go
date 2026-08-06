package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/config"
)

// configSubscriber observes a config value that lives outside the *config.Config
// struct once the app is running — cached in an engine field, an atomic, a
// synced file on disk.
//
// It exists because "declare the key hot" and "the key actually takes effect"
// were separate facts. Consumers snapshot config at construction, and hot
// reload was a hand-maintained list of apply* calls that each new consumer had
// to remember to join. Shipping one hot key (agent.commit_signing) took three
// rounds of live testing because the same value was cached in four independent
// places, each found only after the previous fix was tested against a running
// server. Two other keys — agent.evidence and agent.admission — are marked
// restart-only in the registry purely because their consumers cache them and
// nothing re-invokes the setter.
type configSubscriber struct {
	// Name identifies the subscriber in logs and in the coverage test.
	Name string
	// Paths are the registry paths this subscriber cares about. Empty means
	// every hot apply. A path matches when it, or one of its ancestors, was
	// applied — so "agent.commit_signing" fires on an "agent" apply.
	Paths []string
	// Apply receives the config that was just made live.
	Apply func(config.Config)
}

// wants reports whether any applied path should wake this subscriber.
func (s configSubscriber) wants(applied []string) bool {
	if len(s.Paths) == 0 {
		return true
	}
	for _, want := range s.Paths {
		for _, got := range applied {
			if got == want || isConfigPathPrefix(got, want) {
				return true
			}
		}
	}
	return false
}

// isConfigPathPrefix reports whether applied is an ancestor of want, e.g.
// "agent" is an ancestor of "agent.commit_signing". A hot apply is recorded
// against registry-entry paths, which are coarser than the leaf a subscriber
// names.
func isConfigPathPrefix(applied, want string) bool {
	return len(want) > len(applied) &&
		want[:len(applied)] == applied &&
		want[len(applied)] == '.'
}

// subscribe registers a consumer of reloaded config. Unexported because
// ConfigService is a Wails-bound service: an exported method here would be
// published to the frontend, which has no business registering Go callbacks. Registration is the whole
// contract: a subscriber never needs a bespoke call added to an apply function
// to participate, which is what let four sinks of one key drift apart.
func (s *ConfigService) subscribe(sub configSubscriber) {
	if s == nil || sub.Apply == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, sub)
}

// subscriberNames returns the registered names, for the coverage test.
func (s *ConfigService) subscriberNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.subscribers))
	for _, sub := range s.subscribers {
		names = append(names, sub.Name)
	}
	slices.Sort(names)
	return names
}

// pendingNotify is the set of subscriber callbacks a hot apply owes, captured
// while the config lock is held and invoked after it is released.
//
// It deliberately does NOT carry the config snapshot it was built from.
// Callbacks run outside s.mu, so a second mutation can acquire the lock and
// finish before an earlier pending.run() gets scheduled; replaying a captured
// snapshot would then leave a sink holding an older value than the live
// config. Each callback reads the current config instead, and notifyMu
// serialises them, so whichever order they run in they converge on the newest
// value.
type pendingNotify struct {
	subs []configSubscriber
	svc  *ConfigService
}

// collectSubscribersLocked snapshots the subscribers a hot apply must wake.
// The caller already holds s.mu — invoking Apply under it deadlocks, because a
// subscriber is app-layer code that reads config back through this same
// service. That deadlock is not hypothetical: the first version of this called
// RLock from applyHotChangesLocked and hung every SaveRawConfig to the test
// timeout.
func (s *ConfigService) collectSubscribersLocked(applied []string, _ config.Config) pendingNotify {
	if s == nil {
		return pendingNotify{}
	}
	var subs []configSubscriber
	for _, sub := range s.subscribers {
		if sub.wants(applied) {
			subs = append(subs, sub)
		}
	}
	return pendingNotify{subs: subs, svc: s}
}

// run invokes the captured callbacks against the live config, serialised so
// two concurrent mutations cannot interleave their notifications. Safe on a
// zero value.
func (p pendingNotify) run() {
	if p.svc == nil || len(p.subs) == 0 {
		return
	}
	p.svc.notifyMu.Lock()
	defer p.svc.notifyMu.Unlock()

	p.svc.mu.RLock()
	live := p.svc.cfg
	p.svc.mu.RUnlock()
	if live == nil {
		return
	}
	for _, sub := range p.subs {
		sub.Apply(*live)
	}
}

// notifySubscribers is the lock-free entry point used by unit tests and by any
// caller that does not already hold s.mu.
func (s *ConfigService) notifySubscribers(applied []string, next config.Config) {
	if s == nil {
		return
	}
	s.mu.RLock()
	pending := s.collectSubscribersLocked(applied, next)
	s.mu.RUnlock()
	pending.run()
}
