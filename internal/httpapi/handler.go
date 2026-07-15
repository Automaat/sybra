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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
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
	methods map[string]struct{}
}

// NewService creates a Service that permits only the named methods over HTTP.
func NewService(impl any, methods ...string) Service {
	m := make(map[string]struct{}, len(methods))
	for _, name := range methods {
		m[name] = struct{}{}
	}
	return Service{Impl: impl, methods: m}
}

// Mount registers POST /api/{service}/{method} handlers for every service in
// the registry. Only methods listed in each Service's allowlist are reachable;
// unknown services or non-allowlisted methods return 404.
func Mount(mux *http.ServeMux, services map[string]Service, logger *slog.Logger) {
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		svcName := r.PathValue("service")
		methodName := r.PathValue("method")

		svc, ok := services[svcName]
		if !ok {
			respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown service: %s", svcName))
			return
		}

		if _, allowed := svc.methods[methodName]; !allowed {
			respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown method: %s.%s", svcName, methodName))
			return
		}

		rv := reflect.ValueOf(svc.Impl)
		m := rv.MethodByName(methodName)
		if !m.IsValid() {
			respondError(w, logger, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("unknown method: %s.%s", svcName, methodName))
			return
		}

		mt := m.Type()

		// Cap body size before reading. MaxBytesReader replaces r.Body with a
		// reader that returns an error after MaxRequestBody bytes — protects
		// against a multi-GB POST exhausting server memory.
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)
		// Read body once.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				respondError(w, logger, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "request body too large")
			} else {
				respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, "failed to read request body")
			}
			return
		}

		// Parse JSON array of arguments when body is non-empty.
		var rawArgs []json.RawMessage
		if len(body) > 0 {
			if err := json.Unmarshal(body, &rawArgs); err != nil {
				respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, "invalid JSON in args")
				return
			}
		}

		numIn := mt.NumIn()
		if len(rawArgs) != numIn {
			respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, fmt.Sprintf("%s.%s expects %d args, got %d", svcName, methodName, numIn, len(rawArgs)))
			return
		}

		// Convert each raw JSON arg to the method's expected parameter type.
		in := make([]reflect.Value, numIn)
		for i := range numIn {
			paramType := mt.In(i)
			// Allocate a pointer to the param type so json.Unmarshal can fill it.
			ptr := reflect.New(paramType)
			if err := json.Unmarshal(rawArgs[i], ptr.Interface()); err != nil {
				respondError(w, logger, http.StatusBadRequest, ErrCodeValidation, fmt.Sprintf("arg %d: invalid argument type", i))
				return
			}
			in[i] = ptr.Elem()
		}

		// Call the method.
		out := m.Call(in)

		// Extract error return (last out value if it implements error).
		if len(out) > 0 {
			last := out[len(out)-1]
			if last.Type().Implements(errType) {
				if !last.IsNil() {
					callErr, _ := last.Interface().(error)
					var ce ClientError
					if errors.As(callErr, &ce) {
						respondError(w, logger, ce.HTTPStatus(), codeForStatus(ce.HTTPStatus()), ce.Error())
					} else {
						logger.Warn("httpapi.call.error",
							"service", svcName, "method", methodName, "err", callErr)
						respondError(w, logger, http.StatusInternalServerError, ErrCodeInternal, "internal error")
					}
					return
				}
				out = out[:len(out)-1]
			}
		}

		// No result to encode.
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
	})
}

var errType = reflect.TypeFor[error]()

// codeForStatus maps an HTTP status returned by a ClientError to the
// structured error code included in the JSON response envelope.
func codeForStatus(status int) ErrorCode {
	switch status {
	case http.StatusConflict:
		return ErrCodeConflict
	default:
		return ErrCodeValidation
	}
}
