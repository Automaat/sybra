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

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func setup(t *testing.T) (*http.ServeMux, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	httpapi.Mount(mux, map[string]any{"TestSvc": &testSvc{}}, slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mux, srv
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

func TestHandler_Echo(t *testing.T) {
	_, srv := setup(t)

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
	_, srv := setup(t)

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
	_, srv := setup(t)
	resp := post(t, srv, "TestSvc", "Void")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHandler_ErrorReturn(t *testing.T) {
	_, srv := setup(t)
	resp := post(t, srv, "TestSvc", "FailWith")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boom") {
		t.Fatalf("expected error message, got: %s", body)
	}
}

func TestHandler_UnknownService(t *testing.T) {
	_, srv := setup(t)
	resp := post(t, srv, "NoSvc", "Foo")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_UnknownMethod(t *testing.T) {
	_, srv := setup(t)
	resp := post(t, srv, "TestSvc", "DoesNotExist")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestHandler_ObjIn(t *testing.T) {
	_, srv := setup(t)
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
