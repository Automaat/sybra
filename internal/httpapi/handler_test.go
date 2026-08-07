package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
)

// testSvc is a minimal service used for handler tests.
type testSvc struct{}

func (s *testSvc) Echo(msg string) string { return "echo:" + msg }
func (s *testSvc) Add(a, b int) int       { return a + b }
func (s *testSvc) Multi() (msg string, ok bool, err error) {
	return "ok", true, nil
}
func (s *testSvc) Void()           {}
func (s *testSvc) Fail() error     { return nil }
func (s *testSvc) FailWith() error { return &testError{"boom"} }

// FailWithNotExist mirrors how internal/task.Store.read wraps a missing-file
// read: fmt.Errorf("...: %w", os.ErrNotExist), so errors.Is finds it through
// the same %w chain stripErrorResult walks in production.
func (s *testSvc) FailWithNotExist() error {
	return fmt.Errorf("task nope not found: %w", os.ErrNotExist)
}
func (s *testSvc) ReturnAndFail(v string) (string, error) {
	return "", &testError{v}
}
func (s *testSvc) ObjIn(obj map[string]string) string { return obj["key"] }
func (s *testSvc) ClientFail400() error               { return &testClientError{400, "bad input"} }
func (s *testSvc) ClientFail409() error               { return &testClientError{409, "already running"} }

// AdminOnly is intentionally not in the allowlist — used by rejection tests.
func (s *testSvc) AdminOnly() string { return "admin" }

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// testClientError implements httpapi.ClientError without importing the package.
type testClientError struct {
	status int
	msg    string
}

func (e *testClientError) Error() string   { return e.msg }
func (e *testClientError) HTTPStatus() int { return e.status }

type testAdmissionError struct {
	status int
	msg    string
}

func (e *testAdmissionError) Error() string   { return e.msg }
func (e *testAdmissionError) HTTPStatus() int { return e.status }

func setup(t *testing.T) (*http.ServeMux, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TestSvc": httpapi.NewService(&testSvc{},
			"Echo", "Add", "Multi", "Void", "Fail", "FailWith", "FailWithNotExist", "ReturnAndFail", "ObjIn",
			"ClientFail400", "ClientFail409",
			// AdminOnly is intentionally absent from the allowlist.
		),
	}, logger, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mux, srv, logBuf
}

func post(t *testing.T, srv *httptest.Server, service, method string, args ...any) *http.Response {
	t.Helper()
	var body io.Reader
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
			panic("unreachable")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/"+service+"/"+method, body)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	return resp
}

// decodeErr asserts Content-Type is application/json and decodes the error envelope.
func decodeErr(t *testing.T, resp *http.Response) (message, code string) {
	t.Helper()
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected Content-Type application/json, got %q", ct)
	}
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
		panic("unreachable")
	}
	return env.Error, env.Code
}

func TestHandler_Echo(t *testing.T) {
	_, srv, _ := setup(t)

	resp := post(t, srv, "TestSvc", "Echo", "hello")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var result string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if result != "echo:hello" {
		t.Fatalf("got %q", result)
	}
}

func TestHandler_Add(t *testing.T) {
	_, srv, _ := setup(t)

	resp := post(t, srv, "TestSvc", "Add", 3, 4)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var result int
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if result != 7 {
		t.Fatalf("got %d", result)
	}
}

func TestHandler_MultipleNonErrorReturns(t *testing.T) {
	_, srv, _ := setup(t)

	resp := post(t, srv, "TestSvc", "Multi")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var result []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 return values, got %d", len(result))
	}
	var first string
	if err := json.Unmarshal(result[0], &first); err != nil {
		t.Fatalf("decode first return: %v", err)
		panic("unreachable")
	}
	var second bool
	if err := json.Unmarshal(result[1], &second); err != nil {
		t.Fatalf("decode second return: %v", err)
		panic("unreachable")
	}
	if first != "ok" || !second {
		t.Fatalf("got (%q, %v), want (%q, %v)", first, second, "ok", true)
	}
}

func TestHandler_Void(t *testing.T) {
	_, srv, _ := setup(t)
	resp := post(t, srv, "TestSvc", "Void")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHandler_ErrorReturn(t *testing.T) {
	_, srv, logBuf := setup(t)
	resp := post(t, srv, "TestSvc", "FailWith")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	msg, code := decodeErr(t, resp)
	if msg != "internal error" {
		t.Fatalf("client must not see raw error; got %q", msg)
	}
	if code != string(httpapi.ErrCodeInternal) {
		t.Fatalf("expected code %q, got %q", httpapi.ErrCodeInternal, code)
	}
	// Raw error must appear in server logs, not in client response.
	logs := logBuf.String()
	if !strings.Contains(logs, "httpapi.call.error") {
		t.Fatalf("expected httpapi.call.error in logs; got: %s", logs)
	}
	if !strings.Contains(logs, "boom") {
		t.Fatalf("expected raw error 'boom' in logs; got: %s", logs)
	}
}

func TestHandler_NotExistMapsTo404(t *testing.T) {
	_, srv, logBuf := setup(t)
	resp := post(t, srv, "TestSvc", "FailWithNotExist")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	msg, code := decodeErr(t, resp)
	if code != string(httpapi.ErrCodeNotFound) {
		t.Fatalf("expected code %q, got %q", httpapi.ErrCodeNotFound, code)
	}
	// Callers like the cluster mirror's reconcileMissing branch purely on the
	// HTTP status (404), never the message, so the client-visible message
	// must be sanitized the same way the generic 500 path is — the raw error
	// wraps an *fs.PathError carrying the store's absolute filesystem path,
	// which must never reach an HTTP client.
	if msg != "not found" {
		t.Fatalf("expected a sanitized not-found message, got %q", msg)
	}
	if strings.Contains(msg, "os.ErrNotExist") || strings.Contains(msg, "/") {
		t.Fatalf("client-visible message must not leak the raw error or a filesystem path, got %q", msg)
	}
	if strings.Contains(logBuf.String(), "httpapi.call.error") {
		t.Fatal("a confirmed not-found must not be logged as an internal error")
	}
	if !strings.Contains(logBuf.String(), "task nope not found") {
		t.Fatal("the raw error must still be logged server-side for debugging")
	}
}

func TestHandler_ClientErrorPassthrough(t *testing.T) {
	_, srv, logBuf := setup(t)

	cases := []struct {
		method     string
		wantStatus int
		wantMsg    string
		wantCode   string
	}{
		{"ClientFail400", http.StatusBadRequest, "bad input", string(httpapi.ErrCodeValidation)},
		{"ClientFail409", http.StatusConflict, "already running", string(httpapi.ErrCodeConflict)},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			resp := post(t, srv, "TestSvc", tc.method)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("expected status %d, got %d", tc.wantStatus, resp.StatusCode)
			}
			msg, code := decodeErr(t, resp)
			if msg != tc.wantMsg {
				t.Fatalf("expected client message %q, got %q", tc.wantMsg, msg)
			}
			if code != tc.wantCode {
				t.Fatalf("expected code %q, got %q", tc.wantCode, code)
			}
			// ClientErrors must NOT appear in server logs as internal errors.
			if strings.Contains(logBuf.String(), "httpapi.call.error") {
				t.Fatal("ClientError must not be logged as internal error")
			}
		})
	}
}

func TestHandler_AdmissionHookHonorsReadOnlyMetadata(t *testing.T) {
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TestSvc": httpapi.NewService(&testSvc{}, "Echo", "Add").WithReadOnly("Echo"),
	}, logger, func(_, _ string, meta httpapi.MethodMeta) error {
		if meta.ReadOnly {
			return nil
		}
		return &testAdmissionError{status: http.StatusServiceUnavailable, msg: "draining"}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	readResp := post(t, srv, "TestSvc", "Echo", "ok")
	defer readResp.Body.Close()
	if readResp.StatusCode != http.StatusOK {
		t.Fatalf("Echo status = %d, want 200", readResp.StatusCode)
	}

	writeResp := post(t, srv, "TestSvc", "Add", 1, 2)
	defer writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Add status = %d, want 503", writeResp.StatusCode)
	}
	msg, code := decodeErr(t, writeResp)
	if msg != "draining" {
		t.Fatalf("Add error = %q, want draining", msg)
	}
	if code != string(httpapi.ErrCodeUnavailable) {
		t.Fatalf("Add code = %q, want %q", code, httpapi.ErrCodeUnavailable)
	}
}

func TestHandler_ErrorEnvelope(t *testing.T) {
	_, srv, _ := setup(t)

	cases := []struct {
		name       string
		service    string
		method     string
		args       []any
		wantStatus int
		wantCode   string
	}{
		{
			name: "unknown service", service: "NoSvc", method: "Foo",
			wantStatus: http.StatusNotFound, wantCode: string(httpapi.ErrCodeNotFound),
		},
		{
			name: "blocked method (not in allowlist)", service: "TestSvc", method: "AdminOnly",
			wantStatus: http.StatusNotFound, wantCode: string(httpapi.ErrCodeNotFound),
		},
		{
			name: "non-existent method", service: "TestSvc", method: "DoesNotExist",
			wantStatus: http.StatusNotFound, wantCode: string(httpapi.ErrCodeNotFound),
		},
		{
			name: "bad JSON args", service: "TestSvc", method: "Echo",
			args:       []any{`not-valid-json-array`},
			wantStatus: http.StatusBadRequest, wantCode: string(httpapi.ErrCodeValidation),
		},
		{
			name: "arg count mismatch", service: "TestSvc", method: "Add",
			args:       []any{1},
			wantStatus: http.StatusBadRequest, wantCode: string(httpapi.ErrCodeValidation),
		},
		{
			name: "arg type mismatch", service: "TestSvc", method: "Add",
			args:       []any{"not-a-number", "also-not-a-number"},
			wantStatus: http.StatusBadRequest, wantCode: string(httpapi.ErrCodeValidation),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.name == "bad JSON args" {
				// Send a raw non-JSON body to trigger args decode failure.
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/"+tc.service+"/"+tc.method,
					strings.NewReader("not-json-array"))
				if err != nil {
					t.Fatal(err)
					panic("unreachable")
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
					panic("unreachable")
				}
			} else {
				resp = post(t, srv, tc.service, tc.method, tc.args...)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("expected status %d, got %d", tc.wantStatus, resp.StatusCode)
			}
			_, code := decodeErr(t, resp)
			if code != tc.wantCode {
				t.Fatalf("expected code %q, got %q", tc.wantCode, code)
			}
		})
	}
}

func TestHandler_UnknownService(t *testing.T) {
	_, srv, _ := setup(t)
	resp := post(t, srv, "NoSvc", "Foo")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_UnknownMethod(t *testing.T) {
	_, srv, _ := setup(t)
	resp := post(t, srv, "TestSvc", "DoesNotExist")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandler_BlockedMethod verifies that a method present on the service but
// absent from the allowlist returns 404, not the method's actual result.
// This is a regression test for the allowlist enforcement introduced to prevent
// unauthenticated callers from reaching privileged mutations (e.g. methods that
// persist shell commands later executed via sh -c).
func TestHandler_BlockedMethod(t *testing.T) {
	_, srv, _ := setup(t)
	resp := post(t, srv, "TestSvc", "AdminOnly")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-allowlisted method, got %d", resp.StatusCode)
	}
}

func TestHandler_ObjIn(t *testing.T) {
	_, srv, _ := setup(t)
	resp := post(t, srv, "TestSvc", "ObjIn", map[string]string{"key": "val"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var result string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if result != "val" {
		t.Fatalf("got %q", result)
	}
}

// TestHandler_OversizedBodyRejected verifies the MaxRequestBody cap blocks
// trivial memory-exhaustion attacks. Sending a body larger than the limit
// must short-circuit before io.ReadAll allocates the whole payload — the
// server returns HTTP 4xx (413 Request Entity Too Large per
// http.MaxBytesReader semantics) instead of crashing.
func TestHandler_OversizedBodyRejected(t *testing.T) {
	_, srv, _ := setup(t)

	// Body just over the cap. The framework triggers MaxBytesReader's error
	// during io.ReadAll on the server side.
	oversize := httpapi.MaxRequestBody + 1024
	body := bytes.Repeat([]byte("a"), oversize)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/TestSvc/Echo", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Some Go versions surface the cap as a connection-reset; treat that as
		// the same successful "rejection" outcome.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("oversized body was accepted (status %d); MaxBytesReader cap not enforced", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		_, code := decodeErr(t, resp)
		if code != string(httpapi.ErrCodeTooLarge) {
			t.Fatalf("expected code %q, got %q", httpapi.ErrCodeTooLarge, code)
		}
	}
}
