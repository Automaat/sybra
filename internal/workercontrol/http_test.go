package workercontrol

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestHTTPErrorResponsesAreJSONAndHideInternalDetails(t *testing.T) {
	database := dbtest.SQLite(t)
	service := New(database)

	malformed := httptest.NewRecorder()
	service.Handler().ServeHTTP(malformed, httptest.NewRequest(http.MethodPost, "/worker/v1/register", strings.NewReader("{")))
	if malformed.Code != http.StatusBadRequest || malformed.Header().Get("Content-Type") != "application/json" || malformed.Body.String() != "{\"error\":\"invalid request\"}\n" {
		t.Fatalf("malformed response = %d %q %q", malformed.Code, malformed.Header().Get("Content-Type"), malformed.Body.String())
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	body := `{"workerId":"worker","negotiation":{"protocolMin":{"major":1,"minor":0},"protocolMax":{"major":1,"minor":0},"buildVersion":"test"}}`
	internal := httptest.NewRecorder()
	service.Handler().ServeHTTP(internal, httptest.NewRequest(http.MethodPost, "/worker/v1/register", strings.NewReader(body)))
	if internal.Code != http.StatusInternalServerError || internal.Body.String() != "{\"error\":\"internal server error\"}\n" {
		t.Fatalf("internal response = %d %q", internal.Code, internal.Body.String())
	}
}
