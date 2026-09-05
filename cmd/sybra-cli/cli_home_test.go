package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpserve"
)

// TestHomeFlag_OverridesEverything pins --home as the top of the precedence
// order: it wins even when SYBRA_CONTROL_HOME and SYBRA_HOME both point
// elsewhere. A home now names which board the CLI reaches, so each one here
// runs its own.
func TestHomeFlag_OverridesEverything(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	startTestBoard(t, realHome)
	startTestBoard(t, sandboxHome)
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", filepath.Join(t.TempDir(), "unused"))

	code, out := runCLI(t, "--json", "--home", realHome, "create", "--title", "in real home")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	// The task must land under --home, not SYBRA_HOME or SYBRA_CONTROL_HOME.
	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "in real home") {
		t.Fatalf("list under --home = %q, want the created task", out)
	}

	// The sandbox home (bare SYBRA_HOME, no --home) must not see it.
	code, out = runCLI(t, "--json", "--home", sandboxHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if strings.Contains(out, "in real home") {
		t.Fatalf("sandbox home list leaked the --home task: %s", out)
	}
}

// TestHomeFlag_EqualsForm pins the --home=DIR form alongside --home DIR.
func TestHomeFlag_EqualsForm(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	startTestBoard(t, realHome)
	startTestBoard(t, sandboxHome)
	t.Setenv("SYBRA_HOME", sandboxHome)

	code, out := runCLI(t, "--json", "--home="+realHome, "create", "--title", "equals form")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "equals form") {
		t.Fatalf("list under --home = %q, want the created task", out)
	}
}

// TestControlHomeEnv_WinsOverSybraHome pins the second precedence tier: bare
// sybra-cli (no --home) inside a task-scoped agent, where SYBRA_HOME points at
// the sandbox but SYBRA_CONTROL_HOME points at the real operator store, must
// reach the real store.
func TestControlHomeEnv_WinsOverSybraHome(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	startTestBoard(t, realHome)
	startTestBoard(t, sandboxHome)
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", realHome)

	code, out := runCLI(t, "--json", "create", "--title", "via control home")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if !strings.Contains(out, "via control home") {
		t.Fatalf("real home list = %q, want the task created via SYBRA_CONTROL_HOME", out)
	}

	code, out = runCLI(t, "--json", "--home", sandboxHome, "list")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, out)
	}
	if strings.Contains(out, "via control home") {
		t.Fatalf("sandbox home leaked the SYBRA_CONTROL_HOME task: %s", out)
	}
}

// TestControlHomeEnv_ReachesTheBoardItNames replaces a test that asserted
// SYBRA_CONTROL_HOME forced filesystem mode. Editing files behind the instance
// that owns them is the failure this whole surface removed, so the guarantee is
// now about which board is reached, not about bypassing every board.
func TestControlHomeEnv_ReachesTheBoardItNames(t *testing.T) {
	realHome := t.TempDir()
	sandboxHome := t.TempDir()
	startTestBoard(t, realHome)
	startTestBoard(t, sandboxHome)
	t.Setenv("SYBRA_HOME", sandboxHome)
	t.Setenv("SYBRA_CONTROL_HOME", realHome)

	code, out := runCLI(t, "--json", "create", "--title", "control home target")
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}

	// The board named by SYBRA_CONTROL_HOME holds it; the sandbox board does not.
	code, out = runCLI(t, "--json", "--home", realHome, "list")
	if code != 0 || !strings.Contains(out, "control home target") {
		t.Fatalf("control-home board list = %q (exit %d), want the created task", out, code)
	}
	code, out = runCLI(t, "--json", "--home", sandboxHome, "list")
	if code != 0 {
		t.Fatalf("sandbox list exit %d: %s", code, out)
	}
	if strings.Contains(out, "control home target") {
		t.Fatalf("sandbox board leaked the control-home task: %s", out)
	}
}

// TestBoardCommandRefusesWithNoServer is the contract this issue exists for: a
// command that needs board state and finds no server says so and exits
// non-zero, rather than opening the files behind whichever instance owns them.
func TestBoardCommandRefusesWithNoServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv(serverTargetEnv, "")
	// A port proven closed, rather than the default: this repo's own dev:mock
	// listens on 8080, so relying on the default made the test depend on what
	// else the machine happens to be running.
	writeClosedPortFile(t, home)

	code, _, stderr := runCLIWithStderr(t, "--json", "list")
	if code == 0 {
		t.Fatal("list exit 0 with no server reachable")
	}
	if !refusedToUseATarget(stderr) {
		t.Errorf("stderr = %q, want it to refuse the target it resolved", stderr)
	}
	// Nothing may have been written where the board's files would live.
	if _, err := os.Stat(filepath.Join(home, "tasks")); err == nil {
		t.Error("the refused command created the board's task directory")
	}
}

// TestHomeFlag_MalformedMissingValue_NonHookIsFatal pins that a dangling
// --home with no value is a hard usage error for ordinary commands.
func TestHomeFlag_MalformedMissingValue_NonHookIsFatal(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "list", "--home")
	if code == 0 {
		t.Fatal("expected non-zero exit for a dangling --home with no value")
	}
}

// TestHomeFlag_MalformedMissingValue_HookFailsOpen pins that the same
// malformed --home never blocks a codex hook invocation — hook must always
// exit 0 so a bad flag can't stall an agent run.
func TestHomeFlag_MalformedMissingValue_HookFailsOpen(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "hook", "--home")
	if code != 0 {
		t.Fatalf("hook exit = %d, want 0 (fail-open)", code)
	}
}

// TestInferredTargetAcceptsAnUnidentifiedPeer pins a deliberate trade.
//
// A server older than the home field answers /health with exactly
// {"status":"ok"} — byte-identical to a process that is not Sybra at all, so
// the two cannot be told apart. Refusing it would break every deployment
// between this CLI landing and the server restarting, which auto-update
// coalesces by up to an hour, and every agent's task CRUD runs through this
// path. So silence is accepted and the bearer token remains the real gate.
//
// A peer that positively claims a different home is still refused, and nothing
// that deletes local files accepts silence — see ownsHome.
func TestInferredTargetAcceptsAnUnidentifiedPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv(serverTargetEnv, "")

	var gotAuth string
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Authorization"); v != "" {
			gotAuth = v
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(impostor.Close)

	u, err := url.Parse(impostor.URL)
	if err != nil {
		t.Fatalf("parse impostor URL: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split impostor host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, desktopPortFile), []byte(port), 0o600); err != nil {
		t.Fatalf("write desktop port: %v", err)
	}

	// The board answers, so the command proceeds; what it gets back is the
	// peer's business, and the token is what actually authorises it.
	runCLIWithStderr(t, "--json", "list")
	if gotAuth == "" {
		t.Fatal("refused a board that predates the home field; every deployment breaks on upgrade until its server restarts")
	}
}

// TestInferredTargetRefusesAPeerServingAnotherHome measures the case a bare
// service marker cannot answer: the marker is a fixed public string, so a local
// process can echo it. What it cannot do is claim this home without the
// operator noticing, and an inferred target is trusted on that alone.
//
// The danger is concrete: with no bind configured every home infers the same
// default port, so an isolated SYBRA_HOME would otherwise drive whichever board
// holds it — including the operator's real one.
func TestInferredTargetRefusesAPeerServingAnotherHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_CONTROL_HOME", "")
	t.Setenv(serverTargetEnv, "")

	var gotAuth string
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Authorization"); v != "" {
			gotAuth = v
		}
		_, _ = w.Write([]byte(`{"status":"ok","service":"` + httpserve.ServiceMarker + `","home_id":"` + httpserve.HomeID("/somewhere/else") + `"}`))
	}))
	t.Cleanup(impostor.Close)

	u, err := url.Parse(impostor.URL)
	if err != nil {
		t.Fatalf("parse impostor URL: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split impostor host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, desktopPortFile), []byte(port), 0o600); err != nil {
		t.Fatalf("write desktop port: %v", err)
	}

	code, _, stderr := runCLIWithStderr(t, "--json", "list")
	if code == 0 {
		t.Fatal("list exit 0 against a board serving a different home")
	}
	if gotAuth != "" {
		t.Fatalf("sent %q to a board serving another home", gotAuth)
	}
	if !strings.Contains(stderr, "does not serve") {
		t.Errorf("stderr = %q, want it to say the peer does not serve this home", stderr)
	}
}
