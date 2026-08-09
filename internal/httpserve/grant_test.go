package httpserve

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
)

type stubGrants struct{ token, taskID string }

func (s stubGrants) Verify(token string) (string, bool) {
	if token != "" && token == s.token {
		return s.taskID, true
	}
	return "", false
}

// TestAuthMiddlewareWith_GrantIsAcceptedAndMarkedSandboxed pins the two halves
// of a per-run credential: it authorizes, and it says what kind of caller it
// is. The sandboxed mark is set from the credential rather than from a header
// the caller sets about itself, so an agent cannot claim to be an operator.
func TestAuthMiddlewareWith_GrantIsAcceptedAndMarkedSandboxed(t *testing.T) {
	var sawSandboxed string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawSandboxed = r.Header.Get(httpapi.SandboxedCallerHeader)
	})
	handler := AuthMiddlewareWith("board-token", stubGrants{token: "run-grant", taskID: "task-a"}, discard(), next)

	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer run-grant")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a live grant was refused")
	}
	if sawSandboxed == "" {
		t.Fatal("a grant-authorized request was not marked sandboxed; host-acting methods would be reachable")
	}
}

// TestAuthMiddlewareWith_OperatorTokenIsNotSandboxed keeps the operator's own
// credential reaching the methods that act on this machine.
func TestAuthMiddlewareWith_OperatorTokenIsNotSandboxed(t *testing.T) {
	var sawSandboxed string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawSandboxed = r.Header.Get(httpapi.SandboxedCallerHeader)
	})
	handler := AuthMiddlewareWith("board-token", stubGrants{token: "run-grant", taskID: "task-a"}, discard(), next)

	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer board-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the board's own token was refused")
	}
	if sawSandboxed != "" {
		t.Fatal("the operator's own credential was marked sandboxed")
	}
}

// TestAuthMiddlewareWith_ForgedSandboxHeaderIsOverwritten keeps a caller from
// choosing its own classification in either direction.
func TestAuthMiddlewareWith_ForgedSandboxHeaderIsOverwritten(t *testing.T) {
	var sawSandboxed string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawSandboxed = r.Header.Get(httpapi.SandboxedCallerHeader)
	})
	handler := AuthMiddlewareWith("board-token", stubGrants{token: "run-grant", taskID: "task-a"}, discard(), next)

	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer run-grant")
	// A sandboxed caller trying to pass itself off as an operator.
	req.Header.Del(httpapi.SandboxedCallerHeader)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if sawSandboxed == "" {
		t.Fatal("a grant holder cleared its own sandbox mark")
	}
}

// TestAuthMiddlewareWith_UnknownCredentialIsRefused pins that adding grants did
// not open a second way in.
func TestAuthMiddlewareWith_UnknownCredentialIsRefused(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	handler := AuthMiddlewareWith("board-token", stubGrants{token: "run-grant"}, discard(), next)

	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer neither-one")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("an unknown credential reached the handler (code=%d called=%v)", rec.Code, called)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
