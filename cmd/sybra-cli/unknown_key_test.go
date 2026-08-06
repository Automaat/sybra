package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const staleKeyConfig = "schema_version: 2\nagent:\n  sandbox_read_policy: strict\n"

// writeStaleKeyConfig seeds an isolated home with a config carrying a key this
// build no longer knows — the state a rendered config lands in when a key is
// removed from the schema and the template still ships it.
func writeStaleKeyConfig(t *testing.T) string {
	t.Helper()
	home := setupStore(t)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(staleKeyConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestConfigDoctorReportsUnknownKey is the acceptance test for #3133. `config
// doctor` used to refuse to run on an unknown key — dying on the one input it
// exists to explain, and taking the provider capacity report with it. It must
// report the key as a finding and still perform every other check.
func TestConfigDoctorReportsUnknownKey(t *testing.T) {
	writeStaleKeyConfig(t)

	code, stdout, stderr := runCLIWithStderr(t, "--json", "config", "doctor")
	if code != 1 {
		t.Errorf("exit = %d, want 1 (an unknown key is still an error)", code)
	}

	var report configDoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("config doctor produced no report: %v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}

	var named bool
	for _, f := range report.Findings {
		if f.Severity == "error" && strings.Contains(f.Message, "agent.sandbox_read_policy") {
			named = true
		}
	}
	if !named {
		t.Errorf("no error finding names the unknown key; findings=%+v", report.Findings)
	}
	if report.Routing.ProviderPreference == "" {
		t.Error("routing summary is empty — doctor stopped at the bad key instead of resolving the rest")
	}
	if len(report.Findings) < 2 {
		t.Errorf("only the schema finding was produced, so no other check ran: %+v", report.Findings)
	}
}

// TestConfigSubcommandsStillFailClosedOnUnknownKey pins the other half: only
// the diagnostic tolerates a bad key. Anything that hands the resolved config
// to a caller still refuses, since the key is silently dropped from it.
func TestConfigSubcommandsStillFailClosedOnUnknownKey(t *testing.T) {
	for _, sub := range []string{"dump", "explain"} {
		t.Run(sub, func(t *testing.T) {
			writeStaleKeyConfig(t)

			args := []string{"config", sub}
			if sub == "explain" {
				args = append(args, "agent.provider")
			}
			code, _, stderr := runCLIWithStderr(t, args...)
			if code == 0 {
				t.Fatalf("config %s exited 0 on an unknown config key", sub)
			}
			if !strings.Contains(stderr, "agent.sandbox_read_policy") {
				t.Errorf("stderr does not name the offending key: %q", stderr)
			}
		})
	}
}

// TestUnknownKeyDoesNotRedirectTaskWrites pins the second acceptance criterion.
// A schema error used to drop the CLI into its direct task-store fallback,
// which is exactly the path an agent cannot take: agents run `sybra-cli update`
// from inside a sandboxed worktree where that store is read-only, so one stale
// key became agents that could not update their own tasks.
func TestUnknownKeyDoesNotRedirectTaskWrites(t *testing.T) {
	for _, cmd := range []string{"list", "update"} {
		t.Run(cmd, func(t *testing.T) {
			writeStaleKeyConfig(t)

			args := []string{cmd}
			if cmd == "update" {
				args = append(args, "abc12345", "--status", "done")
			}
			code, _, stderr := runCLIWithStderr(t, args...)
			if code == 0 {
				t.Fatalf("`%s` exited 0 on an unknown config key", cmd)
			}
			if strings.Contains(stderr, "falling back to direct task store") {
				t.Errorf("`%s` fell back to the direct task store on a schema error: %q", cmd, stderr)
			}
			if !strings.Contains(stderr, "agent.sandbox_read_policy") {
				t.Errorf("stderr does not name the offending key: %q", stderr)
			}
		})
	}
}

// TestUnreachableConfigStillFallsBack keeps the fallback alive for what it was
// built for: a config that cannot be read at all, where nothing is known about
// the server and a local task store is the best available answer.
func TestUnreachableConfigStillFallsBack(t *testing.T) {
	home := setupStore(t)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("::not yaml at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLIWithStderr(t, "list")
	if code != 0 {
		t.Fatalf("`list` exited %d; the fallback must still cover an unparseable config: %q", code, stderr)
	}
	if !strings.Contains(stderr, "falling back to direct task store") {
		t.Errorf("no fallback warning for an unparseable config: %q", stderr)
	}
}
