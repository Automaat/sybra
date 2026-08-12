package executioncontract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func TestV1CompatibilityFixturesDecodeAndRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		decode func([]byte) (any, error)
	}{
		{"run", "v1-run-spec.json", func(data []byte) (any, error) { return DecodeRunSpec(data) }},
		{"command", "v1-command.json", func(data []byte) (any, error) { return DecodeCommand(data) }},
		{"event", "v1-event.json", func(data []byte) (any, error) { return DecodeEvent(data) }},
		{"terminal", "v1-terminal.json", func(data []byte) (any, error) { return DecodeTerminalResult(data) }},
		{"artifact", "v1-artifact-manifest.json", func(data []byte) (any, error) { return DecodeArtifactManifest(data) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			first, err := tt.decode(data)
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			encoded, err := json.Marshal(first)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			second, err := tt.decode(encoded)
			if err != nil {
				t.Fatalf("decode round trip: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("round trip mismatch:\nfirst=%#v\nsecond=%#v", first, second)
			}
		})
	}
}

func TestUnsupportedMajorRejectedAcrossMessages(t *testing.T) {
	majorOne := regexp.MustCompile(`"major"\s*:\s*1`)
	fixtures := []struct {
		file   string
		decode func([]byte) error
	}{
		{"v1-run-spec.json", func(data []byte) error { _, err := DecodeRunSpec(data); return err }},
		{"v1-command.json", func(data []byte) error { _, err := DecodeCommand(data); return err }},
		{"v1-event.json", func(data []byte) error { _, err := DecodeEvent(data); return err }},
		{"v1-terminal.json", func(data []byte) error { _, err := DecodeTerminalResult(data); return err }},
		{"v1-artifact-manifest.json", func(data []byte) error { _, err := DecodeArtifactManifest(data); return err }},
	}
	for _, fixture := range fixtures {
		data, err := os.ReadFile(filepath.Join("testdata", fixture.file))
		if err != nil {
			t.Fatal(err)
		}
		data = majorOne.ReplaceAll(data, []byte(`"major":2`))
		if err := fixture.decode(data); !errors.Is(err, ErrUnsupportedMajor) {
			t.Fatalf("%s error = %v, want ErrUnsupportedMajor", fixture.file, err)
		}
	}
}

func TestNegotiationSelectsHighestCompatibleMinor(t *testing.T) {
	got, err := Negotiate(
		Negotiation{ProtocolMin: Version{Major: 1}, ProtocolMax: Version{Major: 1, Minor: 4}, BuildVersion: "leader"},
		Negotiation{ProtocolMin: Version{Major: 1, Minor: 2}, ProtocolMax: Version{Major: 1, Minor: 3}, BuildVersion: "worker"},
	)
	if err != nil || got != (Version{Major: 1, Minor: 3}) {
		t.Fatalf("Negotiate = %+v, %v", got, err)
	}
	if _, err := Negotiate(
		Negotiation{ProtocolMin: Version{Major: 1}, ProtocolMax: Version{Major: 1}},
		Negotiation{ProtocolMin: Version{Major: 2}, ProtocolMax: Version{Major: 2}},
	); !errors.Is(err, ErrUnsupportedMajor) {
		t.Fatalf("major mismatch error = %v", err)
	}
}

func TestValidationRejectsLeaderPathsAndCredentials(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "v1-run-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := DecodeRunSpec(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, badPath := range []string{"/leader/worktree/file", `C:\leader\file`, `..\secrets\token`, `\\server\share`, "../escape", "safe/../secret"} {
		bad := spec
		bad.ExpectedOutputs = append([]ExpectedOutput(nil), spec.ExpectedOutputs...)
		bad.ExpectedOutputs[0].Path = badPath
		if err := bad.Validate(); err == nil {
			t.Fatalf("absolute/traversing path %q accepted", badPath)
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "KUBECONFIG", "NODE_MASTER_KEY"} {
		bad := spec
		bad.Environment = []EnvironmentBinding{{Name: name, SecretRef: &SecretRef{Name: "scoped"}}}
		if err := bad.Validate(); err == nil {
			t.Fatalf("credential %q accepted", name)
		}
	}
	for _, ref := range []string{
		"OPENAI_API_KEY", "SYBRA_SERVER_TOKEN", "../shared/master", "global/provider-key", "run/../shared/grant",
		"run/run-01JREMOTE/SYBRA_SERVER_TOKEN", "run/other-run/grant",
	} {
		bad := spec
		bad.Environment = []EnvironmentBinding{{Name: "SCOPED_INPUT", SecretRef: &SecretRef{Name: ref}}}
		if err := bad.Validate(); err == nil {
			t.Fatalf("unscoped or master credential reference %q accepted", ref)
		}
	}
	bad := spec
	bad.Environment = []EnvironmentBinding{{Name: "DATABASE_PASSWORD", Value: "plaintext"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("inline sensitive environment accepted")
	}
	for _, badRef := range []string{"/leader/private/worktrees/task-1", `C:\leader\main`, "refs/heads/../secret", "refs/heads/main.lock"} {
		bad := spec
		bad.Workspace.BaseRef = badRef
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid git ref %q accepted", badRef)
		}
	}
	bad = spec
	bad.Workspace.Roots = []LogicalRoot{RootWorktree}
	if err := bad.Validate(); err == nil {
		t.Fatal("output under undeclared artifact root accepted")
	}
	for _, clear := range []func(*RunSpec){
		func(spec *RunSpec) { spec.Fence.WorkflowID = "" },
		func(spec *RunSpec) { spec.Fence.StepID = "" },
	} {
		bad := spec
		clear(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("identity-less workflow fence accepted")
		}
	}
}

func TestValidateEventOrderSupportsIdempotentReplay(t *testing.T) {
	now := time.Now().UTC()
	event := func(sequence uint64, key string, typ EventType) EventEnvelope {
		return EventEnvelope{Version: CurrentVersion(), BuildVersion: "test", RunID: "run", Sequence: sequence, EventID: key,
			IdempotencyKey: key, Type: typ, ObservedAt: now}
	}
	if err := ValidateEventOrder([]EventEnvelope{
		event(1, "one", EventStarted), event(1, "one", EventStarted),
		event(2, "two", EventOutput), event(3, "three", EventTerminal), event(3, "three", EventTerminal),
	}); err != nil {
		t.Fatalf("valid replay: %v", err)
	}
	if err := ValidateEventOrder([]EventEnvelope{event(1, "one", EventStarted), event(3, "three", EventOutput)}); err == nil {
		t.Fatal("sequence gap accepted")
	}
	if err := ValidateEventOrder([]EventEnvelope{event(1, "one", EventTerminal), event(2, "two", EventOutput)}); err == nil {
		t.Fatal("post-terminal event accepted")
	}
	for name, mutate := range map[string]func(*EventEnvelope){
		"run":   func(event *EventEnvelope) { event.RunID = "other" },
		"build": func(event *EventEnvelope) { event.BuildVersion = "other" },
		"time":  func(event *EventEnvelope) { event.ObservedAt = event.ObservedAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			first, replay := event(1, "one", EventStarted), event(1, "one", EventStarted)
			mutate(&replay)
			if err := ValidateEventOrder([]EventEnvelope{first, replay}); err == nil {
				t.Fatal("non-exact replay accepted")
			}
		})
	}
}

func TestStartCommandRejectsProcessLocalPayload(t *testing.T) {
	now := time.Now().UTC()
	for _, payload := range []string{
		`{"dir":"/leader/private/worktree","extraEnv":["OPENAI_API_KEY=master"]}`,
		`{"runSpecRef":"run-1","process":{"pid":123}}`,
		`{"runSpecRef":"/leader/private/run-spec.json"}`,
		`{"runSpecRef":"../shared/run-spec"}`,
		`{"runSpecRef":"other-run"}`,
	} {
		command := CommandEnvelope{
			Version: CurrentVersion(), BuildVersion: "test", CommandID: "command", RunID: "run",
			IdempotencyKey: "start", Type: CommandStart, SentAt: now, Payload: json.RawMessage(payload),
		}
		if err := command.Validate(); err == nil {
			t.Fatalf("process-local start payload accepted: %s", payload)
		}
	}
}

func TestTerminalAndArtifactValidationRejectsContradictoryMetadata(t *testing.T) {
	now := time.Now().UTC()
	nonzero := 1
	terminal := TerminalResult{
		Version: CurrentVersion(), BuildVersion: "test", RunID: "run", IdempotencyKey: "terminal",
		State: TerminalSucceeded, LastSequence: 1, ExitCode: &nonzero, ArtifactState: ArtifactsPending, CompletedAt: now,
	}
	if err := terminal.Validate(); err == nil {
		t.Fatal("succeeded result with non-zero exit accepted")
	}
	manifest := ArtifactManifest{
		Version: CurrentVersion(), BuildVersion: "test", RunID: "run", ManifestID: "manifest",
		IdempotencyKey: "manifest", State: ArtifactsReady, GeneratedAt: now,
		Artifacts: []ArtifactEntry{{
			Name: "diff", Kind: "bundle", Root: RootArtifact, Path: "changes/run.bundle",
			DigestSHA256: "x", MediaType: "", Sensitivity: SensitivityInternal,
		}},
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("malformed artifact integrity metadata accepted")
	}
}
