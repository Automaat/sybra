package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
)

// TestLimitGateOrNil_DegradedStoreYieldsGenuineNilInterface guards the
// typed-nil-interface footgun: assigning a nil *limits.Store straight into the
// agent.LimitGate field yields a non-nil interface, which defeats the manager's
// lg == nil guard and panics on first use during degraded limits init. The
// helper must instead return a genuine nil interface.
func TestLimitGateOrNil_DegradedStoreYieldsGenuineNilInterface(t *testing.T) {
	var store *limits.Store // limits.NewStore failed at startup -> nil

	if gate := agent.LimitGateOrNil(store); gate != nil {
		t.Fatal("agent.LimitGateOrNil(nil) must return a genuine nil LimitGate")
	}
}

// TestAgentRuntimeConfig_DegradedLimitsHasNilGate exercises the real wiring
// site: with a degraded (nil) limits store the produced runtime config must
// carry a nil LimitGate so dispatch takes the no-gate path instead of panicking.
func TestAgentRuntimeConfig_DegradedLimitsHasNilGate(t *testing.T) {
	a := &App{} // a.limits nil == degraded limits init
	rc := a.agentRuntimeConfig(&config.Config{})
	if rc.LimitGate != nil {
		t.Fatal("degraded limits store must yield a nil LimitGate so the manager guard fires")
	}
}
