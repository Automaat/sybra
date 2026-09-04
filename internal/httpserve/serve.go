// Package httpserve assembles the HTTP surface both Sybra binaries serve: the
// API dispatcher, the SSE event stream, the health/metrics/pprof endpoints, and
// the SPA that talks to them.
//
// It exists so the desktop app and sybra-server present the same surface rather
// than each growing its own. The UI reaches state through this handler in both,
// so a method added to the service registry is reachable from either build with
// no second wiring step.
package httpserve

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/sse"
)

// ServiceMarker identifies a Sybra control plane in its health response, so a
// client can tell one from whatever else happens to answer on a port.
const ServiceMarker = "sybra"

// HomeID digests the home a board serves, for a client deciding whether that
// board owns the files on this disk.
//
// The digest rather than the path: /health carries no authentication, so the
// path would hand the operator's username and data layout to anyone who can
// reach the port — and to a local process that cannot read the home at all but
// could then echo it back to collect the bearer token. A caller that already
// knows the path can still compare; one that does not, learns nothing.
//
// The path is made absolute before anything else. A relative home digests the
// bare string otherwise, so two processes started from different directories
// with the same relative SYBRA_HOME agree they serve one home while owning
// different disks — and a cleanup then deletes the other's live state.
//
// Symlinks are resolved next, so a home reached through /var and /private/var
// digests the same.
func HomeID(home string) string {
	if strings.TrimSpace(home) == "" {
		return ""
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		resolved = absolute
	}
	sum := sha256.Sum256([]byte(filepath.Clean(resolved)))
	return hex.EncodeToString(sum[:])
}

// Options describes one instance's HTTP surface.
type Options struct {
	Logger *slog.Logger
	Broker *sse.Broker
	// Services is the API dispatch registry, keyed by service name.
	Services map[string]httpapi.Service
	// Admit runs before every dispatched call; nil admits everything the
	// allowlist already permits.
	Admit httpapi.AdmissionFunc
	// StaticDir serves the SPA from a directory on disk. Empty disables it.
	StaticDir string
	// StaticFS serves the SPA from an embedded filesystem, used when the
	// bundle ships inside the binary. Ignored when StaticDir is set.
	StaticFS fs.FS
	// EnablePprof mounts the profiling endpoints behind the same auth.
	EnablePprof bool
	// APIBase is the origin the delivered bundle should call, reported in the
	// runtime config. Empty leaves the bundle on its own origin.
	APIBase string
	// Token is disclosed to a same-origin loopback caller of the runtime
	// config, so a UI on this host need not ask an operator for a secret this
	// process holds. Empty never discloses one.
	Token string
	// SelfOrigin is this instance's own origin, used to tell the page this
	// process served from any other page on the host. Empty disables the
	// token disclosure entirely.
	SelfOrigin string
	// Home is the SYBRA_HOME this instance serves. Its digest, never the path
	// itself, is reported in the health response: a client asking which board
	// owns a directory on this machine cannot tell two instances apart by
	// address alone, since both are loopback.
	Home string
	// Proxy forwards the API and event stream to a board on another machine.
	//
	// The UI stays same-origin with the instance that served it, so no CORS
	// grant is needed on that board — its operator cannot know in advance which
	// loopback port an attaching window will pick — and its bearer token is
	// added here rather than handed to the page.
	Proxy *ProxyTarget
	// WorkerControl serves the durable outbound worker protocol. It is nil on
	// attached desktops and file-backend boards that cannot persist delivery.
	WorkerControl http.Handler
}

// ProxyTarget is the board an attached UI's calls are forwarded to.
type ProxyTarget struct {
	Origin string
	Token  string
}

// BuildMux registers every route this instance serves.
func BuildMux(opts Options) *http.ServeMux {
	mux := http.NewServeMux()

	// Health check endpoint for container orchestration.
	//
	// The service marker is not decoration: a client dials a port it inferred
	// rather than one an operator named, and it sends a bearer token on the
	// next request. Without something identifying the peer, any local process
	// answering 200 here collects that token.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, err := json.Marshal(map[string]string{
			"status":  "ok",
			"service": ServiceMarker,
			"home_id": HomeID(opts.Home),
		})
		if err != nil {
			opts.Logger.Error("health.encode", "err", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(payload)
	})

	// Prometheus scrape endpoint (opt-in via config.metrics.enabled). The
	// OTel Prometheus exporter registers instruments into the default
	// prometheus/client_golang registry, so promhttp.Handler serves them.
	if metrics.Enabled() {
		mux.Handle("GET /metrics", promhttp.Handler())
		opts.Logger.Info("metrics.listen", "path", "/metrics")
	}

	// pprof scrape endpoints. Mounted on the main mux so perf tooling can pull
	// heap / goroutine profiles over the same port without opening a second
	// listener. Off by default to avoid leaking internals on shared deployments.
	if opts.EnablePprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		opts.Logger.Info("pprof.listen", "path", "/debug/pprof/")
	}

	// An attached UI's events come from the board it is attached to, so
	// mountAPI owns /events there. Registering both would panic on the
	// duplicate pattern before the process ever served a request.
	if opts.Broker != nil && opts.Proxy == nil {
		// Multiplexed SSE stream: all events over a single connection.
		mux.HandleFunc("GET /events", opts.Broker.ServeAll)

		// Per-event SSE endpoint (kept for debugging / backward compat).
		mux.HandleFunc("GET /api/events/{eventName}", opts.Broker.ServeHTTP)
	}

	mountAPI(mux, opts)
	if opts.WorkerControl != nil && opts.Proxy == nil {
		// Keep the outer patterns method-specific. A method-agnostic subtree
		// conflicts with the SPA's "GET /" catch-all under Go's ServeMux
		// specificity rules and panics during server startup.
		mux.Handle("GET /worker/v1/", opts.WorkerControl)
		mux.Handle("POST /worker/v1/", opts.WorkerControl)
	}

	mux.HandleFunc("GET /runtime-config.js", runtimeConfig(opts))

	if h := staticHandler(opts); h != nil {
		mux.Handle("GET /", h)
	}

	return mux
}

// mountAPI routes API calls, either straight to this instance's services or,
// for an attached UI, on to the board it belongs to.
func mountAPI(mux *http.ServeMux, opts Options) {
	if opts.Proxy == nil {
		httpapi.Mount(mux, opts.Services, opts.Logger, opts.Admit)
		return
	}
	proxy := newBoardProxy(opts)
	local := http.NewServeMux()
	httpapi.Mount(local, opts.Services, opts.Logger, opts.Admit)

	// Services registered here act on this host and are answered here; every
	// other name belongs to the attached board.
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := opts.Services[apiServiceName(r.URL.Path)]; ok {
			local.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	// Registered per method rather than as a bare "/api/": a pattern matching
	// every method is treated as more general than "GET /", and the two
	// conflict at registration time.
	mux.Handle("POST /api/", route)
	mux.Handle("GET /api/", route)
	mux.Handle("GET /events", proxy)
}

// apiServiceName reports the service segment of /api/{service}/{method}.
func apiServiceName(urlPath string) string {
	rest := strings.TrimPrefix(urlPath, "/api/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// newBoardProxy forwards to the attached board under its own credentials.
//
// An origin that will not parse refuses every request rather than returning a
// proxy that dereferences a nil target and takes the handler goroutine with it
// on the first call.
func newBoardProxy(opts Options) http.Handler {
	target, err := url.Parse(opts.Proxy.Origin)
	if err != nil || target.Host == "" {
		opts.Logger.Error("board.proxy.target", "origin", opts.Proxy.Origin, "err", err)
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "attached board origin is unusable", http.StatusBadGateway)
		})
	}
	token := opts.Proxy.Token
	return &httputil.ReverseProxy{
		// -1 flushes every write straight through, which the event stream
		// needs: a buffered proxy holds events until the buffer fills.
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			// The page authenticated to this instance with this instance's
			// token. Replace it wholesale so the board's own secret is the
			// only one on this hop, and never travels to the page.
			r.Out.Header.Set("Authorization", "Bearer "+token)
			q := r.Out.URL.Query()
			if q.Has("token") {
				q.Del("token")
				r.Out.URL.RawQuery = q.Encode()
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			opts.Logger.Warn("board.proxy.error", "origin", opts.Proxy.Origin, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// runtimeConfig tells the bundle which board it was served by, before it makes
// its first call. index.html loads it ahead of the app script.
//
// The bearer token rides along only for a caller on the loopback interface,
// which is the desktop window talking to its own process. A browser reaching
// this over the network is told the origin and nothing else, and falls back to
// asking its operator for the token.
func runtimeConfig(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := map[string]string{}
		if opts.APIBase != "" {
			cfg["apiBase"] = opts.APIBase
		}
		if opts.Token != "" && loopbackRequest(r) && samePageAsSelf(r, opts.SelfOrigin) {
			cfg["token"] = opts.Token
		}
		payload, err := json.Marshal(cfg)
		if err != nil {
			opts.Logger.Error("runtime_config.encode", "err", err)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprintf(w, "window.__SYBRA_RUNTIME__ = %s;\n", payload)
	}
}

// samePageAsSelf reports that the request came from a page this instance
// served, rather than from any other page the host's browser happens to have
// open.
//
// A classic script tag is exempt from same-origin policy, so without this any
// local page could load this endpoint and read the token out of its own global
// scope — the port is no obstacle once it is stable, and a few hundred failed
// loads find an unstable one.
//
// Sec-Fetch-Site is checked first because a page cannot suppress or forge it,
// while it can drop Referer with referrerpolicy. A caller sending neither is
// not a browser, so it is not the drive-by this guards against, and it already
// had to reach loopback to get here.
func samePageAsSelf(r *http.Request, selfOrigin string) bool {
	if selfOrigin == "" {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		return true
	}
	return referer == selfOrigin || strings.HasPrefix(referer, selfOrigin+"/")
}

// loopbackRequest reports a caller on this host. A proxy on the serving host
// presents every request with a loopback address, so any forwarding header
// disqualifies it — see the same reasoning in internal/httpapi.
func loopbackRequest(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// PprofEnabled reports the opt-in used by both binaries.
func PprofEnabled() bool {
	v := os.Getenv("SYBRA_PPROF")
	return v == "1" || v == "true"
}

func staticHandler(opts Options) http.Handler {
	root := opts.StaticFS
	if opts.StaticDir != "" {
		sub, err := fs.Sub(os.DirFS(opts.StaticDir), ".")
		if err != nil {
			opts.Logger.Error("static.dir", "err", err)
			return nil
		}
		root = sub
	}
	if root == nil {
		return nil
	}
	return SPAHandler{FS: http.FileServer(http.FS(root)), Root: root}
}

// SPAHandler serves static files and falls back to index.html for unknown paths
// (supports client-side routing).
type SPAHandler struct {
	FS http.Handler
	// Root is the bundle, consulted to tell a missing file from an SPA route.
	Root fs.FS
}

func (h SPAHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	if urlPath == "" {
		urlPath = "/"
	}
	if !h.exists(urlPath) {
		// Paths with a file extension (e.g. /favicon.ico) are static asset
		// requests, not SPA routes — return 404 so browsers don't treat an
		// HTML index.html response as a broken asset.
		if strings.Contains(path.Base(urlPath), ".") {
			http.NotFound(w, r)
			return
		}
		r2 := *r
		r2.URL.Path = "/"
		h.FS.ServeHTTP(w, &r2)
		return
	}
	h.FS.ServeHTTP(w, r)
}

// exists asks the bundle, never the host filesystem by name. Joining the
// request path onto a directory string would let "/../etc/passwd" report on
// files outside the bundle; fs.Stat rejects such a name outright.
func (h SPAHandler) exists(urlPath string) bool {
	if h.Root == nil {
		return false
	}
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		name = "."
	}
	_, err := fs.Stat(h.Root, name)
	return err == nil
}

// CSPMiddleware sets the policy the SPA is served under. The page only ever
// calls the origin that served it, including when that origin forwards to a
// board on another machine, so connect-src never needs widening.
func CSPMiddleware(next http.Handler) http.Handler {
	const policy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self' ws: wss:; font-src 'self'; manifest-src 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policy)
		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware echoes CORS headers back only for an Origin present in
// allowedOrigins (exact match) — no wildcard. Requests without a matching
// Origin still reach next (non-browser callers don't need CORS headers at
// all), they just won't be readable cross-origin from an unlisted site.
func CORSMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthMiddleware gates only the HTTP control plane behind a shared-secret
// bearer token: `/api/*`, `/events`, `/api/events/*`, `/metrics`, and
// `/debug/pprof/*`. Browser EventSource cannot set request headers, so the
// SSE endpoints additionally accept the token as a `?token=` query param.
// Static SPA assets stay public so normal browser navigations can load the
// app shell before JS starts issuing authenticated API calls. A blank token
// fails every protected request closed rather than treating it as "auth
// disabled" — config.Load always generates one, so an empty value here means
// misconfiguration, not intent.
func AuthMiddleware(token string, logger *slog.Logger, next http.Handler) http.Handler {
	return AuthMiddlewareWith(token, nil, logger, next)
}

// AuthMiddlewareWith also accepts per-run agent grants.
//
// A request authorized by a grant is stamped as sandboxed before it reaches the
// dispatcher, so the methods that act on the machine serving the board are
// refused for it — decided here from the credential, not from a header the
// caller sets about itself.
func AuthMiddlewareWith(token string, grants GrantVerifier, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !RequestRequiresAuth(r) {
			next.ServeHTTP(w, r)
			return
		}
		// This header is the middleware's own statement about the presented credential, so an inbound copy is cleared first and a caller can never classify itself by setting it.
		r.Header.Del(httpapi.SandboxedCallerHeader)
		authorized, sandboxed := RequestAuthorizedWith(r, token, grants)
		if sandboxed {
			r.Header.Set(httpapi.SandboxedCallerHeader, "1")
		}
		if !authorized {
			logger.Warn("server.auth.denied", "path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="sybra"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized","code":"unauthorized"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequestRequiresAuth reports whether a path is part of the control plane.
func RequestRequiresAuth(r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		return false
	case r.URL.Path == "/events":
		return true
	case r.URL.Path == "/metrics":
		return true
	case strings.HasPrefix(r.URL.Path, "/api/"):
		return true
	case strings.HasPrefix(r.URL.Path, "/worker/v1/"):
		return true
	case strings.HasPrefix(r.URL.Path, "/debug/pprof/"):
		return true
	default:
		return false
	}
}

// RequestAuthorized reports whether the request carries the shared token.
// GrantVerifier resolves a per-run agent credential. Nil means the board
// accepts only its own token, which is every deployment that has not issued a
// grant yet.
type GrantVerifier interface {
	Verify(token string) (taskID string, ok bool)
}

// RequestAuthorizedWith accepts either the board's own token or a live per-run
// grant, and reports which.
//
// The distinction is the point: a grant belongs to an agent working inside one
// task, not to an operator at this machine, so the caller is marked sandboxed
// here rather than by a header the caller sets about itself.
func RequestAuthorizedWith(r *http.Request, token string, grants GrantVerifier) (authorized, sandboxed bool) {
	if RequestAuthorized(r, token) {
		return true, false
	}
	if grants == nil {
		return false, false
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		if !isSSEPath(r.URL.Path) {
			return false, false
		}
		bearer = r.URL.Query().Get("token")
	}
	if bearer == "" {
		return false, false
	}
	if _, ok := grants.Verify(bearer); ok {
		return true, true
	}
	return false, false
}

func RequestAuthorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && TokensEqual(bearer, token) {
		return true
	}
	if isSSEPath(r.URL.Path) {
		if qt := r.URL.Query().Get("token"); qt != "" && TokensEqual(qt, token) {
			return true
		}
	}
	return false
}

func isSSEPath(p string) bool {
	return p == "/events" || strings.HasPrefix(p, "/api/events/")
}

// TokensEqual compares two secrets in time independent of how many leading
// bytes match, so a caller cannot search for the token one byte at a time. It
// does not hide the length: subtle.ConstantTimeCompare returns as soon as the
// lengths differ.
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Handler composes the mux with the middleware chain both binaries serve.
//
// Order matters: CORS must sit outside auth so preflight OPTIONS requests
// (which never carry Authorization) are answered before reaching it.
func Handler(opts Options, token string, allowedOrigins []string) http.Handler {
	return CSPMiddleware(CORSMiddleware(allowedOrigins, AuthMiddleware(token, opts.Logger, BuildMux(opts))))
}
