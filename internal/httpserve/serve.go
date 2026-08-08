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
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/sse"
)

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
	// ConnectOrigin is an additional origin the delivered bundle may call,
	// set when the UI talks to a board on another machine. It is added to the
	// content policy, which would otherwise allow the page's own origin only.
	ConnectOrigin string
}

// BuildMux registers every route this instance serves.
func BuildMux(opts Options) *http.ServeMux {
	mux := http.NewServeMux()

	// Health check endpoint for container orchestration.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
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

	if opts.Broker != nil {
		// Multiplexed SSE stream: all events over a single connection.
		mux.HandleFunc("GET /events", opts.Broker.ServeAll)

		// Per-event SSE endpoint (kept for debugging / backward compat).
		mux.HandleFunc("GET /api/events/{eventName}", opts.Broker.ServeHTTP)
	}

	// API dispatch: POST /api/{service}/{method}
	httpapi.Mount(mux, opts.Services, opts.Logger, opts.Admit)

	mux.HandleFunc("GET /runtime-config.js", runtimeConfig(opts))

	if h := staticHandler(opts); h != nil {
		mux.Handle("GET /", h)
	}

	return mux
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
	if opts.StaticDir != "" {
		sub, err := fs.Sub(os.DirFS(opts.StaticDir), ".")
		if err != nil {
			opts.Logger.Error("static.dir", "err", err)
			return nil
		}
		return SPAHandler{FS: http.FileServer(http.FS(sub)), Dir: opts.StaticDir}
	}
	if opts.StaticFS != nil {
		return SPAHandler{FS: http.FileServer(http.FS(opts.StaticFS)), Embedded: opts.StaticFS}
	}
	return nil
}

// SPAHandler serves static files and falls back to index.html for unknown paths
// (supports client-side routing).
type SPAHandler struct {
	FS http.Handler
	// Dir is the on-disk root, consulted to tell a missing file from an SPA
	// route. Empty when the bundle is embedded.
	Dir string
	// Embedded is the in-binary root, used for the same test when Dir is empty.
	Embedded fs.FS
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

func (h SPAHandler) exists(urlPath string) bool {
	if h.Dir != "" {
		_, err := os.Stat(h.Dir + urlPath)
		return !os.IsNotExist(err)
	}
	if h.Embedded == nil {
		return false
	}
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		name = "."
	}
	_, err := fs.Stat(h.Embedded, name)
	return err == nil
}

// CSPMiddleware sets the policy the SPA is served under. connectOrigin widens
// connect-src to a board on another machine; empty keeps the page to its own
// origin.
func CSPMiddleware(connectOrigin string, next http.Handler) http.Handler {
	connect := "'self' ws: wss:"
	if connectOrigin != "" {
		connect += " " + connectOrigin
	}
	policy := "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src " +
		connect + "; font-src 'self'; manifest-src 'self'; frame-ancestors 'none'"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !RequestRequiresAuth(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !RequestAuthorized(r, token) {
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
	case strings.HasPrefix(r.URL.Path, "/debug/pprof/"):
		return true
	default:
		return false
	}
}

// RequestAuthorized reports whether the request carries the shared token.
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

// TokensEqual compares two secrets without leaking their prefix length.
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Handler composes the mux with the middleware chain both binaries serve.
//
// Order matters: CORS must sit outside auth so preflight OPTIONS requests
// (which never carry Authorization) are answered before reaching it.
func Handler(opts Options, token string, allowedOrigins []string) http.Handler {
	return CSPMiddleware(opts.ConnectOrigin, CORSMiddleware(allowedOrigins, AuthMiddleware(token, opts.Logger, BuildMux(opts))))
}
