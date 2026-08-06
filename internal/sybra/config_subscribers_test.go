package sybra

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

// A subscriber must wake on the coarse registry path a hot apply is recorded
// against, not only on its own leaf. Applies carry entry paths like "agent";
// a subscriber names the leaf it cares about, e.g. "agent.commit_signing".
func TestConfigSubscriber_WantsMatchesAncestorPaths(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		applied []string
		want    bool
	}{
		{"exact leaf", []string{"agent.commit_signing"}, []string{"agent.commit_signing"}, true},
		{"ancestor entry", []string{"agent.commit_signing"}, []string{"agent"}, true},
		{"unrelated sibling", []string{"agent.commit_signing"}, []string{"github"}, false},
		{"no paths means every apply", nil, []string{"anything"}, true},
		// "agentqueue" must not match a subscriber on "agent": prefix
		// matching has to respect the path separator or unrelated keys wake
		// each other.
		{"prefix without separator", []string{"agent.commit_signing"}, []string{"agentqueue"}, false},
		{"one of several paths", []string{"a.x", "b.y"}, []string{"b"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := configSubscriber{Paths: tc.paths, Apply: func(config.Config) {}}
			if got := sub.wants(tc.applied); got != tc.want {
				t.Errorf("wants(%v) with paths %v = %v, want %v", tc.applied, tc.paths, got, tc.want)
			}
		})
	}
}

// The point of the mechanism, driven through the REAL mutation path
// (SaveRawConfig → applyHotChangesLocked). Calling notifySubscribers directly
// proves only that the function works: it leaves the broadcast deletable with
// every test still green, which is exactly what happened on the first attempt
// at this test.
func TestConfigService_SaveRawConfigReachesSubscribers(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	raw := strings.Join([]string{
		"schema_version: 2",
		"agent:",
		"  commit_signing: auto",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	var saw []string
	svc.subscribe(configSubscriber{
		Name:  "commit_signing",
		Paths: []string{"agent.commit_signing"},
		Apply: func(c config.Config) { saw = append(saw, c.CommitSigning()) },
	})

	edited := strings.Join([]string{
		"schema_version: 2",
		"agent:",
		"  commit_signing: never",
		"",
	}, "\n")
	if err := svc.SaveRawConfig(edited); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}

	if len(saw) != 1 || saw[0] != "never" {
		t.Fatalf("subscriber saw %v after a hot reload, want exactly [never]", saw)
	}
}

// Unit-level behaviour of the dispatch itself.
func TestConfigService_HotApplyReachesSubscribers(t *testing.T) {
	svc := &ConfigService{}

	var woke []string
	var sawSigning string
	svc.subscribe(configSubscriber{
		Name:  "commit_signing",
		Paths: []string{"agent.commit_signing"},
		Apply: func(c config.Config) {
			woke = append(woke, "commit_signing")
			sawSigning = c.CommitSigning()
		},
	})
	svc.subscribe(configSubscriber{
		Name:  "unrelated",
		Paths: []string{"github"},
		Apply: func(config.Config) { woke = append(woke, "unrelated") },
	})

	var cfg config.Config
	cfg.Agent.CommitSigning = "never"
	// Callbacks read the live config rather than a captured snapshot, so the
	// service must have one — see pendingNotify.
	svc.cfg = &cfg
	svc.notifySubscribers([]string{"agent"}, cfg)

	if len(woke) != 1 || woke[0] != "commit_signing" {
		t.Fatalf("woke = %v, want only commit_signing", woke)
	}
	if sawSigning != "never" {
		t.Errorf("subscriber saw %q, want the newly-applied never", sawSigning)
	}
	if names := svc.subscriberNames(); len(names) != 2 {
		t.Errorf("subscriberNames() = %v, want both registered", names)
	}
}

// A nil service and a subscriber with no Apply must not panic — App wiring
// registers lazily and standalone tests construct bare services.
func TestConfigService_SubscribeIsNilSafe(t *testing.T) {
	var nilSvc *ConfigService
	nilSvc.subscribe(configSubscriber{Name: "x", Apply: func(config.Config) {}})

	svc := &ConfigService{}
	svc.subscribe(configSubscriber{Name: "no-apply"})
	svc.notifySubscribers([]string{"agent"}, config.Config{})
	if names := svc.subscriberNames(); len(names) != 0 {
		t.Errorf("subscriberNames() = %v, want the Apply-less subscriber rejected", names)
	}
}

// Callbacks run outside the config lock, so a second mutation can finish
// before an earlier notification is scheduled. Replaying a captured snapshot
// would then leave a sink holding an older value than the live config; reading
// the live config at callback time converges regardless of order.
func TestConfigService_ConcurrentMutationsConvergeOnLiveConfig(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	write := func(value string) string {
		return strings.Join([]string{"schema_version: 2", "agent:", "  commit_signing: " + value, ""}, "\n")
	}
	if err := os.WriteFile(cfgPath, []byte(write("auto")), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	var mu sync.Mutex
	var seen []string
	svc.subscribe(configSubscriber{
		Name:  "commit_signing",
		Paths: []string{"agent.commit_signing"},
		Apply: func(c config.Config) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, c.CommitSigning())
		},
	})

	for _, v := range []string{"never", "require", "never"} {
		if err := svc.SaveRawConfig(write(v)); err != nil {
			t.Fatalf("SaveRawConfig(%s): %v", v, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no subscriber callbacks fired")
	}
	// Whatever the interleaving, the final observation must match the live
	// config rather than a stale captured snapshot.
	if last, want := seen[len(seen)-1], svc.cfg.CommitSigning(); last != want {
		t.Errorf("last observed %q, live config is %q", last, want)
	}
}
