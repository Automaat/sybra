package limits

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	claudeOAuthUsageURL = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBeta     = "oauth-2025-04-20"
	claudeUserAgent     = "claude-code/2.1.0"
	liveFetchTimeout    = 15 * time.Second
	keychainTimeout     = 3 * time.Second
)

type claudeCredentials struct {
	ClaudeAIOAuth struct {
		AccessToken      string `json:"accessToken"`
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type codexRPCMessage struct {
	ID     *int            `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type codexRateLimitsResponse struct {
	RateLimits *codexAppServerRateLimits `json:"rateLimits"`
}

type codexAppServerRateLimits struct {
	LimitID              string                     `json:"limitId"`
	LimitName            *string                    `json:"limitName"`
	Primary              *codexAppServerLimitWindow `json:"primary"`
	Secondary            *codexAppServerLimitWindow `json:"secondary"`
	PlanType             string                     `json:"planType"`
	RateLimitReachedType *string                    `json:"rateLimitReachedType"`
}

type codexAppServerLimitWindow struct {
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin int     `json:"windowDurationMins"`
	ResetsAt          int64   `json:"resetsAt"`
}

// RefreshLiveSnapshots polls provider account quota APIs and persists any exact
// snapshots they expose. Missing credentials/CLIs are non-fatal for the other
// provider; callers can log the returned joined error for diagnostics.
func (s *Store) RefreshLiveSnapshots(ctx context.Context, policy Policy) error {
	now := s.now().UTC()
	var snapshots []Snapshot
	var errs []error

	if providerEnabled(policy, ProviderClaude) {
		providerCtx, cancel := context.WithTimeout(ctx, liveFetchTimeout)
		snapshot, ok, err := fetchClaudeLiveSnapshot(providerCtx, now)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("claude: %w", err))
		} else if ok {
			snapshots = append(snapshots, snapshot)
		}
	}

	if providerEnabled(policy, ProviderCodex) {
		providerCtx, cancel := context.WithTimeout(ctx, liveFetchTimeout)
		snapshot, ok, err := fetchCodexLiveSnapshot(providerCtx, now)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("codex: %w", err))
		} else if ok {
			snapshots = append(snapshots, snapshot)
		}
	}

	if len(snapshots) > 0 {
		if err := s.Import(nil, snapshots); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func fetchClaudeLiveSnapshot(ctx context.Context, capturedAt time.Time) (Snapshot, bool, error) {
	credentials, ok, err := readClaudeOAuthCredentials(ctx)
	if err != nil || !ok {
		return Snapshot{}, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeOAuthUsageURL, http.NoBody)
	if err != nil {
		return Snapshot{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.ClaudeAIOAuth.AccessToken)
	req.Header.Set("anthropic-beta", claudeOAuthBeta)
	req.Header.Set("User-Agent", claudeUserAgent)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Snapshot{}, false, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return Snapshot{}, false, fmt.Errorf("usage endpoint returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Snapshot{}, false, err
	}
	snapshot, ok, err := parseClaudeUsageSnapshot(data, capturedAt)
	if !ok || err != nil {
		return snapshot, ok, err
	}
	snapshot.PlanType = credentials.ClaudeAIOAuth.SubscriptionType
	if snapshot.PlanType == "" {
		snapshot.PlanType = credentials.ClaudeAIOAuth.RateLimitTier
	}
	return snapshot, true, nil
}

func readClaudeOAuthCredentials(ctx context.Context) (claudeCredentials, bool, error) {
	if runtime.GOOS == "darwin" {
		if credentials, ok, err := readClaudeCredentialsFromKeychain(ctx, os.Getenv("CLAUDE_CONFIG_DIR")); err != nil || ok {
			return credentials, ok, err
		}
	}
	return readClaudeCredentialsFromFile()
}

func readClaudeCredentialsFromKeychain(ctx context.Context, configDir string) (claudeCredentials, bool, error) {
	for _, service := range claudeKeychainServices(configDir) {
		ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", keychainUser(), "-w")
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		credentials, ok, err := parseClaudeCredentials(bytes.TrimSpace(out))
		if err != nil || ok {
			return credentials, ok, err
		}
	}
	return claudeCredentials{}, false, nil
}

func claudeKeychainServices(configDir string) []string {
	const legacy = "Claude Code-credentials"
	if configDir == "" {
		return []string{legacy}
	}
	hash := sha256.Sum256([]byte(configDir))
	return []string{legacy + "-" + hex.EncodeToString(hash[:])[:8], legacy}
}

func keychainUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	if user := os.Getenv("USERNAME"); user != "" {
		return user
	}
	return "user"
}

func readClaudeCredentialsFromFile() (claudeCredentials, bool, error) {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return claudeCredentials{}, false, err
		}
		configDir = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return claudeCredentials{}, false, nil
		}
		return claudeCredentials{}, false, err
	}
	return parseClaudeCredentials(data)
}

func parseClaudeCredentials(data []byte) (claudeCredentials, bool, error) {
	var credentials claudeCredentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return claudeCredentials{}, false, err
	}
	if credentials.ClaudeAIOAuth.AccessToken == "" {
		return claudeCredentials{}, false, nil
	}
	return credentials, true, nil
}

func parseClaudeUsageSnapshot(data []byte, capturedAt time.Time) (Snapshot, bool, error) {
	var usage claudeUsageResponse
	if err := json.Unmarshal(data, &usage); err != nil {
		return Snapshot{}, false, err
	}
	primary, hasPrimary, err := claudeUsageCycle(usage.FiveHour, 300)
	if err != nil {
		return Snapshot{}, false, err
	}
	secondary, hasSecondary, err := claudeUsageCycle(usage.SevenDay, 10080)
	if err != nil {
		return Snapshot{}, false, err
	}
	if !hasPrimary && !hasSecondary {
		return Snapshot{}, false, nil
	}
	return Snapshot{
		Provider:   ProviderClaude,
		LimitID:    ProviderClaude,
		Primary:    primary,
		Secondary:  secondary,
		Source:     SourceLivePoll,
		Confidence: ConfidenceExact,
		CapturedAt: capturedAt,
	}, true, nil
}

func claudeUsageCycle(raw *claudeUsageWindow, windowMinutes int) (*CycleSnapshot, bool, error) {
	if raw == nil || raw.Utilization == nil {
		return nil, false, nil
	}
	out := &CycleSnapshot{
		UsedPercent:   clampPercent(*raw.Utilization),
		WindowMinutes: windowMinutes,
	}
	if raw.ResetsAt != nil && *raw.ResetsAt != "" {
		resetsAt, err := time.Parse(time.RFC3339Nano, *raw.ResetsAt)
		if err != nil {
			return nil, false, err
		}
		out.ResetsAt = resetsAt.UTC()
	}
	return out, true, nil
}

func fetchCodexLiveSnapshot(ctx context.Context, capturedAt time.Time) (Snapshot, bool, error) {
	cmd := exec.CommandContext(ctx, "codex", "-s", "read-only", "-a", "untrusted", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Snapshot{}, false, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Snapshot{}, false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Snapshot{}, false, err
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	sendRPC := func(id int, method string, params any) error {
		payload := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdin, "%s\n", data)
		return err
	}
	sendNotification := func(method string) error {
		data, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  method,
			"params":  map[string]any{},
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdin, "%s\n", data)
		return err
	}

	if err := sendRPC(1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "sybra", "version": "0.0.0"},
	}); err != nil {
		return Snapshot{}, false, err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerBuffer)
	rateLimitsRequested := false
	for scanner.Scan() {
		var msg codexRPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue
		}
		if msg.Error != nil {
			return Snapshot{}, false, fmt.Errorf("rpc error: %s", msg.Error.Message)
		}
		if *msg.ID == 1 && !rateLimitsRequested {
			if err := sendNotification("initialized"); err != nil {
				return Snapshot{}, false, err
			}
			if err := sendRPC(2, "account/rateLimits/read", map[string]any{}); err != nil {
				return Snapshot{}, false, err
			}
			rateLimitsRequested = true
			continue
		}
		if *msg.ID == 2 {
			return parseCodexAppServerSnapshot(msg.Result, capturedAt)
		}
	}
	if err := scanner.Err(); err != nil {
		return Snapshot{}, false, err
	}
	if stderr.Len() > 0 {
		return Snapshot{}, false, fmt.Errorf("codex app-server exited: %s", stderr.String())
	}
	return Snapshot{}, false, errors.New("codex app-server exited before rate limits response")
}

func parseCodexAppServerSnapshot(data []byte, capturedAt time.Time) (Snapshot, bool, error) {
	var response codexRateLimitsResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return Snapshot{}, false, err
	}
	limits := response.RateLimits
	if limits == nil {
		return Snapshot{}, false, nil
	}
	snapshot := Snapshot{
		Provider:             ProviderCodex,
		PlanType:             limits.PlanType,
		LimitID:              limits.LimitID,
		Primary:              codexAppServerCycle(limits.Primary),
		Secondary:            codexAppServerCycle(limits.Secondary),
		RateLimitReachedType: ptrString(limits.RateLimitReachedType),
		Source:               SourceLivePoll,
		Confidence:           ConfidenceExact,
		CapturedAt:           capturedAt,
	}
	if limits.LimitName != nil {
		snapshot.LimitName = *limits.LimitName
	}
	if snapshot.LimitID == "" {
		snapshot.LimitID = ProviderCodex
	}
	return snapshot, snapshot.Primary != nil || snapshot.Secondary != nil, nil
}

func codexAppServerCycle(raw *codexAppServerLimitWindow) *CycleSnapshot {
	if raw == nil {
		return nil
	}
	out := &CycleSnapshot{
		UsedPercent:   clampPercent(raw.UsedPercent),
		WindowMinutes: raw.WindowDurationMin,
	}
	if raw.ResetsAt > 0 {
		out.ResetsAt = time.Unix(raw.ResetsAt, 0).UTC()
	}
	return out
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
