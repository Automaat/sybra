// Package httpapi provides a reflection-based HTTP dispatcher that maps
// POST /api/{service}/{method} to exported methods on registered service objects.
//
// Request body: JSON array of positional arguments (omit body for zero-arg calls).
// Response body: JSON-encoded return value (empty body for void returns).
// Errors: HTTP 500 with plain-text error message.
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
)

// Mount registers POST /api/{service}/{method} handlers for every exported
// method on each service in the registry. Unknown services/methods return 404.
func Mount(mux *http.ServeMux, services map[string]any, logger *slog.Logger) {
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		svcName := r.PathValue("service")
		methodName := r.PathValue("method")

		svc, ok := services[svcName]
		if !ok {
			http.Error(w, fmt.Sprintf("unknown service: %s", svcName), http.StatusNotFound)
			return
		}

		rv := reflect.ValueOf(svc)
		m := rv.MethodByName(methodName)
		if !m.IsValid() {
			http.Error(w, fmt.Sprintf("unknown method: %s.%s", svcName, methodName), http.StatusNotFound)
			return
		}

		mt := m.Type()

		// Read body once.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Parse JSON array of arguments when body is non-empty.
		var rawArgs []json.RawMessage
		if len(body) > 0 {
			if err := json.Unmarshal(body, &rawArgs); err != nil {
				http.Error(w, "decode args: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		numIn := mt.NumIn()
		if len(rawArgs) != numIn {
			http.Error(w, fmt.Sprintf("%s.%s expects %d args, got %d", svcName, methodName, numIn, len(rawArgs)), http.StatusBadRequest)
			return
		}

		// Convert each raw JSON arg to the method's expected parameter type.
		in := make([]reflect.Value, numIn)
		for i := range numIn {
			paramType := mt.In(i)
			// Allocate a pointer to the param type so json.Unmarshal can fill it.
			ptr := reflect.New(paramType)
			if err := json.Unmarshal(rawArgs[i], ptr.Interface()); err != nil {
				http.Error(w, fmt.Sprintf("arg %d: %s", i, err.Error()), http.StatusBadRequest)
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
					logger.Warn("httpapi.call.error",
						"service", svcName, "method", methodName, "err", callErr)
					http.Error(w, callErr.Error(), http.StatusInternalServerError)
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
		if err := json.NewEncoder(w).Encode(out[0].Interface()); err != nil {
			logger.Error("httpapi.encode", "service", svcName, "method", methodName, "err", err)
		}
	})
}

var errType = reflect.TypeFor[error]()
