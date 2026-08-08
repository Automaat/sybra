// Package httpapi provides a reflection-based HTTP dispatcher that maps
// POST /api/{service}/{method} to explicitly allowlisted methods on registered
// service objects.
//
// Request body: JSON array of positional arguments (omit body for zero-arg calls).
// Response body: JSON-encoded return value (empty body for void returns).
// Errors: JSON {"error","code"} envelope; HTTP status reflects the failure class;
// internal (5xx) errors are sanitized — the raw error appears only in server logs.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
)

// MaxRequestBody caps the size of a single API request body. Sybra service
// methods are JSON-arg-typed (small payloads — task IDs, titles, plan text);
// 32 MiB is generous headroom for plan content while preventing trivial
// memory-exhaustion attacks via multi-GB POST bodies. Override with
// SYBRA_HTTPAPI_MAX_BODY_MB at process start if a larger payload is needed.
const MaxRequestBody = 32 * 1024 * 1024

// Service bundles an implementation with its HTTP-accessible method allowlist.
// Only method names present in the allowlist are callable via the HTTP API;
// all other exported methods return 404.
type Service struct {
	Impl    any
	methods map[string]MethodMeta
}

// MethodMeta carries per-method HTTP metadata used by admission hooks.
type MethodMeta struct {
	ReadOnly bool
	// LocalOnly restricts the method to callers on the loopback interface.
	// These open a GUI application or run a CLI on the host the process sits
	// on, so they are meaningful to a client sharing that host and are an
	// arbitrary local action to anyone else.
	LocalOnly bool
}

// AdmissionFunc decides whether a registered service method may run.
// It runs after allowlist validation and before request-body parsing.
type AdmissionFunc func(service, method string, meta MethodMeta) error

// NewService creates a Service that permits only the named methods over HTTP.
func NewService(impl any, methods ...string) Service {
	m := make(map[string]MethodMeta, len(methods))
	for _, name := range methods {
		m[name] = MethodMeta{}
	}
	return Service{Impl: impl, methods: m}
}

// Methods returns the allowlisted method names. Exposed so a test can walk
// the real HTTP surface instead of a hand-maintained copy of it, which is how
// a newly added endpoint gets held to the same contract as its siblings.
func (s Service) Methods() []string {
	out := make([]string, 0, len(s.methods))
	for name := range s.methods {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// WithReadOnly marks the named allowlisted methods as read-only.
func (s Service) WithReadOnly(methods ...string) Service {
	for _, name := range methods {
		meta, ok := s.methods[name]
		if !ok {
			continue
		}
		meta.ReadOnly = true
		s.methods[name] = meta
	}
	return s
}

// WithLocalOnly marks the named allowlisted methods as reachable only from the
// loopback interface. The desktop UI reaches its own in-process server that
// way, so a window attached to a board on another machine simply finds the
// method refused rather than opening an editor on the board's host.
func (s Service) WithLocalOnly(methods ...string) Service {
	for _, name := range methods {
		meta, ok := s.methods[name]
		if !ok {
			continue
		}
		meta.LocalOnly = true
		s.methods[name] = meta
	}
	return s
}

// Mount registers POST /api/{service}/{method} handlers for every service in
// the registry. Only methods listed in each Service's allowlist are reachable;
// unknown services or non-allowlisted methods return 404.
func Mount(mux *http.ServeMux, services map[string]Service, logger *slog.Logger, admit AdmissionFunc) {
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		svcName, methodName, svc, _, ok := admittedMethod(w, logger, r, services, admit)
		if !ok {
			return
		}
		call, rawArgs, ok := decodeCall(w, logger, r, svcName, methodName, svc)
		if !ok {
			return
		}
		out, ok := invoke(r.Context(), call, rawArgs, w, logger, svcName, methodName)
		if !ok {
			return
		}
		writeResponse(w, logger, svcName, methodName, out)
	})
}

func admittedMethod(w http.ResponseWriter, logger *slog.Logger, r *http.Request, services map[string]Service, admit AdmissionFunc) (svcName, methodName string, svc Service, meta MethodMeta, ok bool) {
	svcName = r.PathValue("service")
	methodName = r.PathValue("method")

	svc, ok = services[svcName]
	if !ok {
		respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown service: %s", svcName))
		return "", "", Service{}, MethodMeta{}, false
	}
	meta, ok = svc.methods[methodName]
	if !ok {
		respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown method: %s.%s", svcName, methodName))
		return "", "", Service{}, MethodMeta{}, false
	}
	if meta.LocalOnly && !fromLoopback(r) {
		logger.Warn("httpapi.local_only.denied", "service", svcName, "method", methodName, "remote", r.RemoteAddr)
		respondError(w, logger, http.StatusForbidden, ErrCodeForbidden,
			fmt.Sprintf("%s.%s runs on the host serving this board and is reachable only from it", svcName, methodName))
		return "", "", Service{}, MethodMeta{}, false
	}
	if admit != nil {
		if err := admit(svcName, methodName, meta); err != nil {
			respondAdmissionError(w, logger, svcName, methodName, err)
			return "", "", Service{}, MethodMeta{}, false
		}
	}
	return svcName, methodName, svc, meta, true
}

// forwardedHeaders are the hops a reverse proxy adds. A proxy on the serving
// host presents every request with a loopback RemoteAddr, so the address alone
// would admit the whole LAN to a local-only method. Any of these present means
// the request crossed a proxy and its origin is not this host.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"}

func fromLoopback(r *http.Request) bool {
	for _, h := range forwardedHeaders {
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

func respondAdmissionError(w http.ResponseWriter, logger *slog.Logger, svcName, methodName string, err error) {
	var ce ClientError
	if errors.As(err, &ce) {
		respondError(w, logger, ce.HTTPStatus(), codeForStatus(ce.HTTPStatus()), ce.Error())
		return
	}
	logger.Warn("httpapi.admission.error", "service", svcName, "method", methodName, "err", err)
	respondError(w, logger, http.StatusInternalServerError, ErrCodeInternal, "internal error")
}

func decodeCall(w http.ResponseWriter, logger *slog.Logger, r *http.Request, svcName, methodName string, svc Service) (reflect.Value, []json.RawMessage, bool) {
	call := reflect.ValueOf(svc.Impl).MethodByName(methodName)
	if !call.IsValid() {
		respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown method: %s.%s", svcName, methodName))
		return reflect.Value{}, nil, false
	}
	rawArgs, ok := readArgs(w, logger, r)
	if !ok {
		return reflect.Value{}, nil, false
	}
	return call, rawArgs, true
}

func readArgs(w http.ResponseWriter, logger *slog.Logger, r *http.Request) ([]json.RawMessage, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			respondError(w, logger, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "request body too large")
		} else {
			respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, "failed to read request body")
		}
		return nil, false
	}
	if len(body) == 0 {
		return nil, true
	}
	var rawArgs []json.RawMessage
	if err := json.Unmarshal(body, &rawArgs); err != nil {
		respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, "invalid JSON in args")
		return nil, false
	}
	return rawArgs, true
}

func invoke(ctx context.Context, call reflect.Value, rawArgs []json.RawMessage, w http.ResponseWriter, logger *slog.Logger, svcName, methodName string) ([]reflect.Value, bool) {
	args, ok := decodeInputs(ctx, call.Type(), rawArgs, w, logger, svcName, methodName)
	if !ok {
		return nil, false
	}
	out := call.Call(args)
	return stripErrorResult(out, w, logger, svcName, methodName)
}

// decodeInputs maps the JSON argument array onto the method signature. A
// leading context.Context is supplied from the request rather than the body, so
// a method already written against a context is callable over HTTP without a
// wrapper — the same shape Wails binds in-process.
func decodeInputs(ctx context.Context, mt reflect.Type, rawArgs []json.RawMessage, w http.ResponseWriter, logger *slog.Logger, svcName, methodName string) ([]reflect.Value, bool) {
	offset := 0
	if mt.NumIn() > 0 && mt.In(0) == contextType {
		offset = 1
	}
	wantArgs := mt.NumIn() - offset
	if len(rawArgs) != wantArgs {
		respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, fmt.Sprintf("%s.%s expects %d args, got %d", svcName, methodName, wantArgs, len(rawArgs)))
		return nil, false
	}

	in := make([]reflect.Value, 0, mt.NumIn())
	if offset == 1 {
		in = append(in, reflect.ValueOf(ctx))
	}
	for i := range wantArgs {
		ptr := reflect.New(mt.In(i + offset))
		if err := json.Unmarshal(rawArgs[i], ptr.Interface()); err != nil {
			respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, fmt.Sprintf("arg %d: invalid argument type", i))
			return nil, false
		}
		in = append(in, ptr.Elem())
	}
	return in, true
}

func stripErrorResult(out []reflect.Value, w http.ResponseWriter, logger *slog.Logger, svcName, methodName string) ([]reflect.Value, bool) {
	if len(out) == 0 {
		return out, true
	}
	last := out[len(out)-1]
	if !last.Type().Implements(errType) {
		return out, true
	}
	if last.IsNil() {
		return out[:len(out)-1], true
	}
	callErr, _ := last.Interface().(error)
	var ce ClientError
	switch {
	case errors.As(callErr, &ce):
		respondError(w, logger, ce.HTTPStatus(), codeForStatus(ce.HTTPStatus()), ce.Error())
	case errors.Is(callErr, os.ErrNotExist):
		// A missing-file error (e.g. task.Store.Get on a trashed/deleted task)
		// is a normal, expected outcome for callers like the cluster mirror's
		// reconcileMissing, which needs to tell "the follower confirms this
		// task is gone" apart from "the follower is unreachable" to reconcile
		// its own stale copy instead of leaving it dangling forever. Surfacing
		// it as 404 lets those callers branch on http.StatusNotFound instead of
		// treating every GetTask failure as an opaque, non-retryable 500. The
		// raw error is logged server-side only — like the default 500 case
		// below, it wraps an *fs.PathError carrying the store's absolute
		// filesystem path, which must never reach an HTTP client.
		logger.Info("httpapi.call.not_found", "service", svcName, "method", methodName, "err", callErr)
		respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, "not found")
	default:
		logger.Warn("httpapi.call.error", "service", svcName, "method", methodName, "err", callErr)
		respondError(w, logger, http.StatusInternalServerError, ErrCodeInternal, "internal error")
	}
	return nil, false
}

func writeResponse(w http.ResponseWriter, logger *slog.Logger, svcName, methodName string, out []reflect.Value) {
	if len(out) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	result := out[0].Interface()
	if len(out) > 1 {
		results := make([]any, len(out))
		for i := range out {
			results[i] = out[i].Interface()
		}
		result = results
	}
	if err := json.NewEncoder(w).Encode(result); err != nil {
		logger.Error("httpapi.encode", "service", svcName, "method", methodName, "err", err)
	}
}

var errType = reflect.TypeFor[error]()

var contextType = reflect.TypeFor[context.Context]()

// codeForStatus maps an HTTP status returned by a ClientError to the
// structured error code included in the JSON response envelope.
func codeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusConflict:
		return ErrCodeConflict
	case http.StatusServiceUnavailable:
		return ErrCodeUnavailable
	case http.StatusNotFound:
		return ErrCodeNotFound
	default:
		return ErrCodeValidation
	}
}
