package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
)

// localSvc mirrors the shape of the methods that act on the host serving the
// board: an editor opener and a call that already takes a context.
type localSvc struct{ opened string }

func (s *localSvc) OpenInEditor(path string) error {
	s.opened = path
	return nil
}

func (s *localSvc) WithContext(ctx context.Context, name string) (string, error) {
	if ctx == nil {
		return "", errNilContext
	}
	return "hello " + name, nil
}

type constError string

func (e constError) Error() string { return string(e) }

const errNilContext = constError("nil context")

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func localMux(svc *localSvc) *http.ServeMux {
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"LocalService": httpapi.NewService(svc, "OpenInEditor", "WithContext").
			WithLocalOnly("OpenInEditor"),
	}, testLogger(), nil)
	return mux
}

// TestLocalOnlyMethodRejectsNonLoopback is the containment guard: a UI attached
// to a board on another machine must not be able to open an editor on the host
// running it.
func TestLocalOnlyMethodRejectsNonLoopback(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		forwarded  string
		wantStatus int
	}{
		{name: "loopback", remote: "127.0.0.1:4000", wantStatus: http.StatusOK},
		{name: "loopback v6", remote: "[::1]:4000", wantStatus: http.StatusOK},
		{name: "lan", remote: "192.168.20.5:4000", wantStatus: http.StatusForbidden},
		{name: "proxied through loopback", remote: "127.0.0.1:4000", forwarded: "203.0.113.9", wantStatus: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &localSvc{}
			req := httptest.NewRequest(http.MethodPost, "/api/LocalService/OpenInEditor", strings.NewReader(`["/tmp/x"]`))
			req.RemoteAddr = tc.remote
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			rec := httptest.NewRecorder()
			localMux(svc).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			wantOpened := ""
			if tc.wantStatus == http.StatusOK {
				wantOpened = "/tmp/x"
			}
			if svc.opened != wantOpened {
				t.Fatalf("opened = %q, want %q", svc.opened, wantOpened)
			}
		})
	}
}

// TestLocalOnlyDoesNotRestrictOtherMethods keeps the gate scoped: marking one
// method must not quietly close the rest of the service to the network.
func TestLocalOnlyDoesNotRestrictOtherMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/LocalService/WithContext", strings.NewReader(`["world"]`))
	req.RemoteAddr = "192.168.20.5:4000"
	rec := httptest.NewRecorder()
	localMux(&localSvc{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestContextArgComesFromRequest covers a method already written against a
// context: the dispatcher supplies it, so the JSON body carries only the
// caller's own arguments and no wrapper is needed to expose it.
func TestContextArgComesFromRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/LocalService/WithContext", strings.NewReader(`["world"]`))
	req.RemoteAddr = "127.0.0.1:4000"
	rec := httptest.NewRecorder()
	localMux(&localSvc{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `"hello world"` {
		t.Fatalf("body = %s, want %q", got, `"hello world"`)
	}
}

// TestContextArgIsNotCountedInArgArity pins the error an operator sees when
// they do pass the context slot, so a stale caller gets the real arity rather
// than a decode failure blamed on their first argument.
func TestContextArgIsNotCountedInArgArity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/LocalService/WithContext", strings.NewReader(`[null,"world"]`))
	req.RemoteAddr = "127.0.0.1:4000"
	rec := httptest.NewRecorder()
	localMux(&localSvc{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "expects 1 args, got 2") {
		t.Fatalf("body = %s, want arity message", rec.Body.String())
	}
}
