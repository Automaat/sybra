package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
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
	"github.com/Automaat/sybra/internal/httpapi"
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

// serverCAEnv names the certificate to pin a TLS board against. A board serving
// TLS signs its own certificate, so the system roots reject it; a caller that
// cannot read the operator's copy — an agent under the process sandbox — is
// handed one it can.
const serverCAEnv = "SYBRA_SERVER_CA"

type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
	// probeErr is why the last reachability probe failed, when it failed for a
	// reason worth telling an operator about.
	probeErr error
	// sandboxed records that the credential came from a file the runner wrote
	// into an agent's sandbox home, which is the one caller that is not an
	// operator at a terminal. Requests then decline the methods that act on
	// the serving host — opening an editor, a terminal, or a worktree there.
	sandboxed bool
	// boardService is what the peer called itself in its health response, or
	// "" when it named nothing. See reachable.
	boardService string
	// boardHomeID digests the SYBRA_HOME the board reported serving, learned
	// during the reachability probe. Empty until that probe has run.
	boardHomeID string
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
		// This machine's own TLS board keeps its token in this machine's
		// config, so requiring SYBRA_SERVER_TOKEN would dead-end the escape
		// hatch every refusal recommends. Only a board elsewhere needs the
		// environment to carry one.
		localToken := ""
		if cfg.ServesTLS() && targetIsLoopback(raw) {
			localToken = strings.TrimSpace(cfg.Server.AuthToken)
		}
		c, err := newTLSAPIClient(raw, source, localToken)
		if err != nil {
			return nil, err
		}
		// A named https target that is this machine's own TLS board still needs
		// the pin: its certificate is self-signed, so the system roots reject
		// it and the escape hatch every refusal recommends would dead-end.
		//
		// SYBRA_SERVER_CA first. An agent cannot read the operator config this
		// path would otherwise take the certificate from — it is not on the
		// sandbox read allowlist — so the runner copies the certificate into
		// the sandbox home and names it here.
		certFile := strings.TrimSpace(os.Getenv(serverCAEnv))
		if certFile == "" && !c.remote && cfg.ServesTLS() {
			certFile = cfg.Cluster.TLS.CertFile
		}
		if certFile != "" {
			transport, tlsErr := pinnedTransport(certFile)
			if tlsErr != nil {
				return nil, tlsErr
			}
			c.http = &http.Client{Transport: transport}
		}
		return c, nil
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
	// cfg.Cluster directly, not ListenAddrs: that helper honours
	// SYBRA_BIND_ADDR, and an env var naming a listen address must not steer a
	// client — the bearer token goes wherever this points.
	binds := make([]string, 0, len(cfg.Cluster.BindAddrs)+1)
	for _, addr := range cfg.Cluster.BindAddrs {
		if strings.TrimSpace(addr) != "" {
			binds = append(binds, addr)
		}
	}
	if addr := strings.TrimSpace(cfg.Cluster.BindAddr); addr != "" {
		binds = append(binds, addr)
	}
	// Loopback first among several: the client always prefers a bind it can
	// reach without putting the token on a network hop, and picking merely the
	// first listed skipped a usable loopback bind behind a LAN one.
	configured := ""
	for _, addr := range binds {
		if host, _, err := net.SplitHostPort(addr); err == nil && isLoopbackHost(host) {
			configured = addr
			break
		}
	}
	if configured == "" && len(binds) > 0 {
		configured = binds[0]
	}
	if configured == "" {
		return localOrigin(cfg, "127.0.0.1", config.DefaultServerPort)
	}
	host, port, err := net.SplitHostPort(configured)
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
// boardTokenFromFile reads the credential a sandboxed agent is given, or "" when
// none was named. It is a file rather than an env var because argv and the
// environment of a process are readable by anything running as the same user.
func boardTokenFromFile() (token string, named bool, err error) {
	path := strings.TrimSpace(os.Getenv("SYBRA_AUTH_TOKEN_FILE"))
	if path == "" {
		return "", false, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", true, fmt.Errorf("read %s: %w", "SYBRA_AUTH_TOKEN_FILE", readErr)
	}
	return strings.TrimSpace(string(data)), true, nil
}

func newTLSAPIClient(raw, source, localToken string) (*apiClient, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%s=%q is not a bare https origin", source, raw)
	}
	if path := strings.TrimSpace(u.EscapedPath()); path != "" && path != "/" {
		return nil, fmt.Errorf("%s=%q must carry no path", source, raw)
	}
	// An explicit remote-board token wins over any ambient sandbox token file:
	// a named target on another machine is operator intent, while the file is
	// a local-board credential the runner may have injected for a sandboxed
	// agent.
	token := strings.TrimSpace(os.Getenv(serverTokenEnv))
	fromFile := false
	if token == "" {
		var err error
		token, fromFile, err = boardTokenFromFile()
		if err != nil {
			return nil, err
		}
	}
	if token == "" && localToken != "" {
		// This machine's own TLS board keeps its token in this machine's
		// config; only another machine's does not.
		token = localToken
	}
	if token == "" {
		return nil, fmt.Errorf("%s=%q requires %s or SYBRA_AUTH_TOKEN_FILE", source, raw, serverTokenEnv)
	}
	host, _, splitErr := net.SplitHostPort(u.Host)
	if splitErr != nil {
		host = u.Host
	}
	if fromFile && !isLoopbackHost(host) {
		return nil, fmt.Errorf("%s=%q is not loopback; a sandbox token file may only target this machine's board", source, raw)
	}
	return &apiClient{
		baseURL:   "https://" + u.Host,
		token:     token,
		http:      &http.Client{},
		sandboxed: fromFile,
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
		if source != serverTargetEnv {
			// Inferred, so naming the env var would send an operator to unset
			// something they never set. The cause is the configured bind.
			return nil, fmt.Errorf("this home's only configured bind %q is neither loopback nor TLS, so there is no route to it that keeps the bearer token off the wire; add a loopback entry to cluster.bind_addrs, or configure cluster.tls", raw)
		}
		return nil, fmt.Errorf("%s=%q is not loopback; use https:// with %s rather than sending the token in cleartext",
			source, raw, serverTokenEnv)
	}
	token, fromFile, err := boardTokenFromFile()
	if err != nil {
		return nil, err
	}
	if !fromFile {
		token = strings.TrimSpace(cfg.Server.AuthToken)
	}
	if token == "" {
		return nil, fmt.Errorf("%s=%q needs an auth token in config or SYBRA_AUTH_TOKEN_FILE", source, raw)
	}
	if !fromFile && cfg.ServesTLS() {
		return nil, fmt.Errorf("%s=%q is cleartext but this instance serves TLS", source, raw)
	}
	return &apiClient{
		baseURL:   "http://" + net.JoinHostPort(host, port),
		token:     token,
		http:      &http.Client{},
		sandboxed: fromFile,
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

// reachable reports that something answered the health probe with a document a
// board could have written, and records what it claimed.
//
// It deliberately does not require the service marker. A server older than the
// home field answers exactly {"status":"ok"}, which no check can tell from a
// process that is not Sybra at all, and refusing it would break every agent's
// task CRUD between this CLI landing and that server restarting. What keeps
// the bearer token off an unrelated process is the home the peer claims —
// see servesThisHome, and ownsHome for the paths that delete files.
func (c *apiClient) reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Kept so a refusal can say "certificate does not match" rather than
		// reporting a running server as stopped.
		c.probeErr = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		Service string `json:"service"`
		HomeID  string `json:"home_id"`
	}
	if json.Unmarshal(body, &health) != nil {
		return false
	}
	c.boardHomeID = health.HomeID
	c.boardService = health.Service
	return true
}

// isBoard reports a peer an inferred target may commit to.
//
// A peer that names no service is accepted: a server older than these fields
// answers exactly {"status":"ok"}, which cannot be told from a process that is
// not Sybra at all, and refusing it would break every agent's task CRUD until
// that server restarts. A peer that names something *else* is a different
// matter — it has identified itself as not being a board, and the verifier
// control channel is exactly that: it answers on loopback and serves two
// methods for one task, so a client that commits to it sends the board's token
// and then 404s on everything it asks for.
func (c *apiClient) isBoard() bool {
	if c == nil {
		return false
	}
	return c.boardService == "" || c.boardService == httpserve.ServiceMarker
}

// servesThisHome reports a peer an inferred target may be used for.
//
// A board that names a different home is refused: with no bind configured every
// home infers the same default port, so otherwise an isolated SYBRA_HOME
// reaches whichever board holds it, including the operator's real one.
//
// A board that names no home at all is accepted. That is a server older than
// this field, and refusing it would break every deployment between the CLI
// landing and the server restarting — which auto-update coalesces by up to an
// hour, during which every agent's task CRUD would fail. The bearer token
// remains the actual gate, and ownsHome still requires a positive match before
// anything deletes local files.
func (c *apiClient) servesThisHome(home string) bool {
	if c == nil {
		return false
	}
	if c.boardHomeID == "" {
		return true
	}
	return c.ownsHome(home)
}

// ownsHome reports a board that positively claims this home. Unlike
// servesThisHome it never accepts silence: a server too old to answer the
// question cannot authorise deleting this machine's files.
func (c *apiClient) ownsHome(home string) bool {
	want := httpserve.HomeID(home)
	if c == nil || c.boardHomeID == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.boardHomeID), []byte(want)) == 1
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
	if c.sandboxed {
		// An agent holds the board's token but is not the operator at this
		// machine, and the local-only methods act on the host serving the
		// board. Declared so the board refuses them.
		req.Header.Set(httpapi.SandboxedCallerHeader, "1")
	}
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
		c, err := newTLSAPIClient(target, "this machine's board", strings.TrimSpace(cfg.Server.AuthToken))
		if err != nil {
			return nil, err
		}
		transport, tlsErr := pinnedTransport(cfg.Cluster.TLS.CertFile)
		if tlsErr != nil {
			return nil, tlsErr
		}
		c.http = &http.Client{Transport: transport}
		return c, nil
	}
	return newCleartextAPIClient(cfg, target, "this machine's board")
}

// pinnedTransport verifies the control plane by the fingerprint of the exact
// certificate it was configured to serve, the way the leader verifies a
// follower (internal/cluster/tls.go).
//
// Chain and hostname verification are both wrong here. The certificate is
// self-signed, so no root accepts it; and `cluster gen-cert` mints it for the
// hosts an operator names — a tailnet name and a LAN address — while the client
// dials loopback, so the name would never match either. Identity is the key,
// not the address it answered on.
func pinnedTransport(certFile string) (*http.Transport, error) {
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read the control plane certificate %q: %w", certFile, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("the control plane certificate %q holds no PEM block", certFile)
	}
	want := sha256.Sum256(block.Bytes)

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not *http.Transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the control plane presented no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				return fmt.Errorf("control plane certificate does not match %q", certFile)
			}
			return nil
		},
	}
	return tr, nil
}

// targetIsLoopback reports an https origin that names this machine.
func targetIsLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host, _, splitErr := net.SplitHostPort(u.Host)
	if splitErr != nil {
		host = u.Host
	}
	return isLoopbackHost(host)
}
