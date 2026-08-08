package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpserve"
)

// apiCallTimeout bounds an ordinary board call. It is deliberately short: a
// CRUD call that has not answered in fifteen seconds is not going to.
const apiCallTimeout = 15 * time.Second

// apiSlowCallTimeout bounds the whole-operation endpoints. Umbrella expansion
// and triage classification run a model on the server, so the ordinary ceiling
// would expire on the client while the server completed the work — leaving the
// operator to re-run and race a second expansion against the first.
const apiSlowCallTimeout = 30 * time.Minute

// apiCloneTimeout bounds a synchronous project create. It sits above the
// store's own ten-minute ceiling on the clone it waits for, so the server's
// timeout is the one that fires and its reason is the one reported.
const apiCloneTimeout = 12 * time.Minute

const maxAPIResponseBody = 32 << 20

// errNoServerTarget reports that no board was configured, which is the one
// case a command may answer from this machine's files without saying so.
var errNoServerTarget = errors.New("no server target configured")

// serverTargetEnv is the explicit control-plane target for CLI API mode.
// Reusing SYBRA_PORT/SYBRA_HOST here is unsafe: they describe where the server
// listens, and ambient unit-shell exports made the CLI hit unrelated localhost
// servers during ordinary filesystem-backed commands.
const serverTargetEnv = "SYBRA_SERVER_TARGET"

// serverTokenEnv carries the bearer token for a board on another machine.
// The local path reads the token from config or SYBRA_AUTH_TOKEN_FILE, but a
// remote board's token is not in this machine's config by definition.
const serverTokenEnv = "SYBRA_SERVER_TOKEN"

type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
	// remote records that the target is another machine's board. A loopback server shares this machine's home, so the filesystem stores are the same board and falling back to them is correct; a remote one is a different board, and falling back edits files its owner never reads.
	remote bool
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server returned %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("server returned %d: %s", e.Status, e.Message)
}

// newAPIClient resolves SYBRA_SERVER_TARGET into a client.
//
// A target that is set but unusable is a configuration error, never a silent
// "no server". Treating it as unset is what let an operator point the CLI at a
// board, get no client, and edit this machine's files believing they had
// reached the other one — the failure this whole surface exists to remove.
//
// It returns errNoServerTarget only when no target is configured at all.
func newAPIClient(cfg *config.Config) (*apiClient, error) {
	raw, source := strings.TrimSpace(os.Getenv(serverTargetEnv)), serverTargetEnv
	if raw == "" {
		return nil, errNoServerTarget
	}
	if cfg == nil {
		return nil, fmt.Errorf("%s is set but no configuration is loaded", serverTargetEnv)
	}
	if strings.HasPrefix(raw, "https://") {
		return newTLSAPIClient(raw, source, "")
	}
	return newCleartextAPIClient(cfg, raw, source)
}

// desktopPortFile is where the desktop app records the loopback port it serves
// its board on. It is the only way to find that board: the port is chosen at
// startup and appears in no configuration file.
const desktopPortFile = "desktop-port"

// localBoardCandidates lists the boards this machine might be running, most
// likely first, so an operator who named no target still reaches the instance
// that owns their home rather than editing its files underneath it.
//
// It is a list rather than one answer because the desktop app's recorded port
// outlives the process: the file is kept deliberately, so the port survives a
// restart, which means a stale entry must not shadow a server that is actually
// up. Every candidate is probed before any is used.
func localBoardCandidates(cfg *config.Config) []string {
	var out []string
	if port := readDesktopPort(); port != "" {
		out = append(out, localOrigin(cfg, "127.0.0.1", port))
	}
	if target := configuredServerTarget(cfg); target != "" {
		out = append(out, target)
	}
	return out
}

func readDesktopPort() string {
	data, err := os.ReadFile(filepath.Join(config.HomeDir(), desktopPortFile))
	if err != nil {
		return ""
	}
	port := strings.TrimSpace(string(data))
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return ""
	}
	return port
}

// configuredServerTarget reports where sybra-server listens on this machine.
//
// SYBRA_HOST/SYBRA_PORT are deliberately not consulted: they say where a server
// should listen, and an ambient export from an unrelated unit shell would aim
// the CLI — and the bearer token it sends next — at whatever answers there.
// Only the configured bind counts.
func configuredServerTarget(cfg *config.Config) string {
	addrs, _ := cfg.ListenAddrs("", "")
	if len(addrs) == 0 {
		return localOrigin(cfg, "127.0.0.1", config.DefaultServerPort)
	}
	host, port, err := net.SplitHostPort(addrs[0])
	if err != nil || strings.TrimSpace(port) == "" {
		return localOrigin(cfg, "127.0.0.1", config.DefaultServerPort)
	}
	// A wildcard or empty bind answers on loopback; a concrete one does not, so
	// an operator who locked the control plane to one interface is dialled
	// there rather than at a loopback address nothing is listening on.
	if strings.TrimSpace(host) == "" || isWildcardHost(host) {
		host = "127.0.0.1"
	}
	return localOrigin(cfg, host, port)
}

// localOrigin renders a candidate in the scheme this instance serves. A TLS
// control plane refuses a cleartext hop, so inferring an http:// target for one
// would make every command fail on a deployment that configured it.
func localOrigin(cfg *config.Config, host, port string) string {
	if cfg.ServesTLS() {
		return "https://" + net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, port)
}

// newTLSAPIClient targets a board over TLS. The token has to come from the
// environment: a board on another machine does not keep its token in this
// machine's config, by definition.
func newTLSAPIClient(raw, source, localToken string) (*apiClient, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s=%q is not a bare https origin", source, raw)
	}
	if path := strings.TrimSpace(u.EscapedPath()); path != "" && path != "/" {
		return nil, fmt.Errorf("%s=%q must carry no path", source, raw)
	}
	token := strings.TrimSpace(os.Getenv(serverTokenEnv))
	if token == "" && localToken != "" {
		// This machine's own TLS board keeps its token in this machine's
		// config; only another machine's does not.
		token = localToken
	}
	if token == "" {
		return nil, fmt.Errorf("%s=%q requires %s", source, raw, serverTokenEnv)
	}
	host, _, splitErr := net.SplitHostPort(u.Host)
	if splitErr != nil {
		host = u.Host
	}
	return &apiClient{
		baseURL: "https://" + u.Host,
		token:   token,
		http:    &http.Client{},
		// A TLS board on this machine is still this machine's board, so the
		// filesystem stores remain a correct fallback for it.
		remote: !isLoopbackHost(host),
	}, nil
}

// newCleartextAPIClient targets a board over plain HTTP, which only a loopback
// origin may be. Anything else would put the bearer token on the wire.
func newCleartextAPIClient(cfg *config.Config, raw, source string) (*apiClient, error) {
	host, port, ok := parseServerTarget(raw)
	if !ok {
		return nil, fmt.Errorf("%s=%q is not a valid host:port or http origin", source, raw)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("%s=%q is not loopback; use https:// with %s rather than sending the token in cleartext",
			serverTargetEnv, raw, serverTokenEnv)
	}
	token := strings.TrimSpace(cfg.Server.AuthToken)
	tokenPath := strings.TrimSpace(os.Getenv("SYBRA_AUTH_TOKEN_FILE"))
	if tokenPath != "" {
		data, err := os.ReadFile(tokenPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", "SYBRA_AUTH_TOKEN_FILE", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return nil, fmt.Errorf("%s=%q needs an auth token in config or SYBRA_AUTH_TOKEN_FILE", source, raw)
	}
	if tokenPath == "" && cfg.ServesTLS() {
		return nil, fmt.Errorf("%s=%q is cleartext but this instance serves TLS", source, raw)
	}
	return &apiClient{
		baseURL: "http://" + net.JoinHostPort(host, port),
		token:   token,
		http:    &http.Client{},
	}, nil
}

func parseServerTarget(raw string) (host, port string, ok bool) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", "", false
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "http" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		if path := strings.TrimSpace(u.EscapedPath()); path != "" && path != "/" {
			return "", "", false
		}
		target = u.Host
	}
	h, p, err := net.SplitHostPort(target)
	if err != nil {
		return "", "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", "", false
	}
	if strings.TrimSpace(h) == "" || isWildcardHost(h) {
		return "", "", false
	}
	return h, p, true
}

func isWildcardHost(h string) bool {
	return h == "0.0.0.0" || h == "::"
}

func isLoopbackHost(h string) bool {
	h = strings.Trim(strings.TrimSpace(h), "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// reachable reports that a Sybra control plane answers, not merely that
// something does.
//
// The identity check is what keeps the bearer token off an unrelated process:
// the next request carries it, and a port this client inferred rather than an
// operator named may belong to anything.
func (c *apiClient) reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		Service string `json:"service"`
	}
	return json.Unmarshal(body, &health) == nil && health.Service == httpserve.ServiceMarker
}

func (c *apiClient) call(ctx context.Context, service, method string, out any, args ...any) error {
	return c.callWithin(ctx, apiCallTimeout, service, method, out, args...)
}

// callWithin bounds one request. The client's own http.Client carries no
// Timeout, so the deadline is the context's alone and a slow endpoint is not
// cut off by a ceiling meant for CRUD.
func (c *apiClient) callWithin(ctx context.Context, timeout time.Duration, service, method string, out any, args ...any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/"+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return parseAPIError(resp.StatusCode, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

func parseAPIError(status int, body []byte) *apiError {
	e := &apiError{Status: status, Message: strings.TrimSpace(string(body))}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		e.Message = env.Error
		e.Code = env.Code
	}
	return e
}

func viaAPI[T any](api *apiClient, service, method string, args ...any) (result T, handled bool, err error) {
	if api == nil {
		return result, false, nil
	}
	callErr := api.call(context.Background(), service, method, &result, args...)
	if callErr == nil {
		return result, true, nil
	}
	var ae *apiError
	if errors.As(callErr, &ae) {
		return result, true, callErr
	}
	return result, false, nil
}

// newLocalAPIClient builds a client for a board inferred for this machine.
//
// Its failures are not an operator's mistake — nobody named this target — so
// the caller skips a candidate it cannot build rather than reporting it, and
// the commands that run without a server keep running.
func newLocalAPIClient(cfg *config.Config, target string) (*apiClient, error) {
	if cfg == nil || strings.TrimSpace(target) == "" {
		return nil, errNoServerTarget
	}
	if strings.HasPrefix(target, "https://") {
		return newTLSAPIClient(target, "this machine's board", strings.TrimSpace(cfg.Server.AuthToken))
	}
	return newCleartextAPIClient(cfg, target, "this machine's board")
}
