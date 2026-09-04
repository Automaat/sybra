package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staleKeyShapes covers every position an unknown key can sit in, because they
// are rejected by two different passes: a key inside a schema v2 namespace is
// refused by the document normalizer, and everything else by the schema walk.
// A fixture that only exercises one pass reports green while the other still
// kills the CLI outright.
var staleKeyShapes = []struct {
	name string
	yaml string
	key  string
}{
	{"top level", "schema_version: 2\nfuture_namespace:\n  enabled: true\n", "future_namespace"},
	{"execution namespace", "schema_version: 2\nexecution:\n  bogus:\n    enabled: true\n", "execution.bogus"},
	{"workflow namespace", "schema_version: 2\nworkflow:\n  bogus:\n    enabled: true\n", "workflow.bogus"},
	{"integrations namespace", "schema_version: 2\nintegrations:\n  slack:\n    enabled: true\n", "integrations.slack"},
	{"supervision namespace", "schema_version: 2\nsupervision:\n  bogus:\n    enabled: true\n", "supervision.bogus"},
	{"observability namespace", "schema_version: 2\nobservability:\n  bogus:\n    enabled: true\n", "observability.bogus"},
	{"storage namespace", "schema_version: 2\nstorage:\n  bogus:\n    enabled: true\n", "storage.bogus"},
	{"storage paths namespace", "schema_version: 2\nstorage:\n  paths:\n    bogus: /tmp/x\n", "storage.paths.bogus"},
	{"instance namespace", "schema_version: 2\ninstance:\n  bogus: true\n", "instance.bogus"},
	{"legacy flat key", "schema_version: 2\nagent:\n  sandbox_read_policy: strict\n", "agent.sandbox_read_policy"},
	{"nested under a known key", "schema_version: 2\nexecution:\n  agent:\n    sandbox_read_policy: strict\n", "agent.sandbox_read_policy"},
}

// writeStaleKeyConfigYAML seeds an isolated home with a config carrying a key
// this build no longer knows — the state a rendered config lands in when a key
// is removed from the schema and the template still ships it.
func writeStaleKeyConfigYAML(t *testing.T, contents string) string {
	t.Helper()
	home := setupStore(t)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestConfigDoctorReportsUnknownKey is the acceptance test for #3133. `config
// doctor` used to refuse to run on an unknown key — dying on the one input it
// exists to explain, and taking the provider capacity report with it. It must
// report the key as a finding and still perform every other check.
func TestConfigDoctorReportsUnknownKey(t *testing.T) {
	for _, shape := range staleKeyShapes {
		t.Run(shape.name, func(t *testing.T) {
			writeStaleKeyConfigYAML(t, shape.yaml)

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
				if f.Severity == "error" && strings.Contains(f.Message, shape.key) {
					named = true
				}
			}
			if !named {
				t.Errorf("no error finding names %q; findings=%+v", shape.key, report.Findings)
			}
			if report.Routing.ProviderPreference == "" {
				t.Error("routing summary is empty — doctor stopped at the bad key instead of resolving the rest")
			}
			if len(report.Findings) < 2 {
				t.Errorf("only the schema finding was produced, so no other check ran: %+v", report.Findings)
			}
		})
	}
}

// TestConfigDoctorReportsEveryUnknownKey pins the one-pass contract: template
// drift leaves several stale keys at once, and reporting one per run turns a
// single fix into a sequence of them.
func TestConfigDoctorReportsEveryUnknownKey(t *testing.T) {
	writeStaleKeyConfigYAML(t, "schema_version: 2\nintegrations:\n  slack:\n    enabled: true\nstorage:\n  bogus: true\nfuture_namespace:\n  enabled: true\n")

	code, stdout, _ := runCLIWithStderr(t, "--json", "config", "doctor")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	var report configDoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("config doctor produced no report: %v", err)
	}
	for _, key := range []string{"integrations.slack", "storage.bogus", "future_namespace"} {
		var named bool
		for _, f := range report.Findings {
			if f.Severity == "error" && strings.Contains(f.Message, key) {
				named = true
			}
		}
		if !named {
			t.Errorf("no error finding names %q; findings=%+v", key, report.Findings)
		}
	}
}

// TestConfigSubcommandsStillFailClosedOnUnknownKey pins the other half: only
// the diagnostic tolerates a bad key. Anything that hands the resolved config
// to a caller still refuses, since the key is silently dropped from it.
func TestConfigSubcommandsStillFailClosedOnUnknownKey(t *testing.T) {
	for _, sub := range []string{"dump", "explain"} {
		for _, shape := range staleKeyShapes {
			t.Run(sub+"/"+shape.name, func(t *testing.T) {
				writeStaleKeyConfigYAML(t, shape.yaml)

				args := []string{"config", sub}
				if sub == "explain" {
					args = append(args, "agent.provider")
				}
				code, _, stderr := runCLIWithStderr(t, args...)
				if code == 0 {
					t.Fatalf("config %s exited 0 on an unknown config key", sub)
				}
				if !strings.Contains(stderr, shape.key) {
					t.Errorf("stderr does not name the offending key: %q", stderr)
				}
			})
		}
	}
}

// TestUnknownKeyDoesNotRedirectTaskWrites pins the second acceptance criterion.
// A schema error used to drop the CLI into its direct task-store fallback,
// which is exactly the path an agent cannot take: agents run `sybra-cli update`
// from inside a sandboxed worktree where that store is read-only, so one stale
// key became agents that could not update their own tasks.
func TestUnknownKeyDoesNotRedirectTaskWrites(t *testing.T) {
	for _, cmd := range []string{"list", "update"} {
		for _, shape := range staleKeyShapes {
			t.Run(cmd+"/"+shape.name, func(t *testing.T) {
				writeStaleKeyConfigYAML(t, shape.yaml)

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
				if !strings.Contains(stderr, shape.key) {
					t.Errorf("stderr does not name the offending key: %q", stderr)
				}
			})
		}
	}
}

// TestUnreachableConfigStillFallsBack keeps the fallback alive for what it was
// built for: a config that cannot be read at all, where nothing is known about
// the server and a local task store is the best available answer.
// TestUnparseableConfigFailsRatherThanEditingFiles replaces a test that
// required an unparseable config to fall back to the task store. That fallback
// is gone: a config the CLI cannot read says nothing about whether the board is
// up, and opening its files was the silent second writer this issue removed.
func TestUnparseableConfigFailsRatherThanEditingFiles(t *testing.T) {
	home := setupStore(t)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("::not yaml at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runCLIWithStderr(t, "list")
	if code == 0 {
		t.Fatal("`list` exited 0 with a config it cannot parse")
	}
	if !strings.Contains(stderr, "load config") {
		t.Errorf("stderr = %q, want it to name the config failure", stderr)
	}
	if strings.Contains(stderr, "falling back") {
		t.Errorf("stderr = %q; the CLI must not fall back to the board's files", stderr)
	}
}

// TestJSONErrorIsParseable pins that --json failures stay machine-readable.
// Agents run `sybra-cli --json update` per the sybra-tasks skill, and the
// unknown-key messages this change routes to them quote the offending key —
// interpolating that into a JSON string literal produced output no agent could
// parse.
func TestJSONErrorIsParseable(t *testing.T) {
	writeStaleKeyConfigYAML(t, staleKeyShapes[0].yaml)

	_, _, stderr := runCLIWithStderr(t, "--json", "update", "abc12345", "--status", "done")
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &got); err != nil {
		t.Fatalf("--json error is not parseable JSON: %v\nstderr=%q", err, stderr)
	}
	if !strings.Contains(got["error"], staleKeyShapes[0].key) {
		t.Errorf("error field = %q, want it to name the offending key", got["error"])
	}
}
