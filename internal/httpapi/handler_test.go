package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
)

// testSvc is a minimal service used for handler tests.
type testSvc struct{}

func (s *testSvc) Echo(msg string) string { return "echo:" + msg }
func (s *testSvc) Add(a, b int) int       { return a + b }
func (s *testSvc) Void()                  {}
func (s *testSvc) Fail() error            { return nil }
func (s *testSvc) FailWith() error        { return &testError{"boom"} }
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

func setup(t *testing.T) (*http.ServeMux, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, nil))
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]httpapi.Service{
		"TestSvc": httpapi.NewService(&testSvc{},
			"Echo", "Add", "Void", "Fail", "FailWith", "ReturnAndFail", "ObjIn",
			"ClientFail400", "ClientFail409",
			// AdminOnly is intentionally absent from the allowlist.
		),
	}, logger)
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
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/"+service+"/"+method, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
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
	}
	if result != 7 {
		t.Fatalf("got %d", result)
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
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
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
