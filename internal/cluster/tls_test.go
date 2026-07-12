package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestParsePin(t *testing.T) {
	good := strings.Repeat("ab", 32)
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"plain hex", good, false},
		{"sha256 prefix", "sha256:" + good, false},
		{"colon separated", strings.Join(splitPairs(good), ":"), false},
		{"uppercase", strings.ToUpper(good), false},
		{"too short", "abcd", true},
		{"not hex", strings.Repeat("zz", 32), true},
		{"empty", "", true},
	}
	for _, c := range cases {
		_, err := ParsePin(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: ParsePin err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func splitPairs(s string) []string {
	var out []string
	for i := 0; i+2 <= len(s); i += 2 {
		out = append(out, s[i:i+2])
	}
	return out
}

func TestClientTLSPinAcceptAndReject(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, _ *http.Request) {
		_ = writeJSON(w, []task.Task{{ID: "pinned"}})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(srv.Certificate().Raw)
	correctPin := hex.EncodeToString(sum[:])

	accept, err := NewClient(Node{Name: "tls", Endpoints: []string{srv.URL}, TLSPin: correctPin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := accept.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("correct pin should connect: %v", err)
	}
	if len(got) != 1 || got[0].ID != "pinned" {
		t.Fatalf("ListTasks = %+v", got)
	}

	wrongPin := strings.Repeat("00", 32)
	reject, err := NewClient(Node{Name: "tls", Endpoints: []string{srv.URL}, TLSPin: wrongPin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reject.ListTasks(context.Background()); err == nil {
		t.Fatal("wrong pin must reject the connection")
	}
}

func TestNewClientInvalidPin(t *testing.T) {
	if _, err := NewClient(Node{Name: "bad", Endpoints: []string{"https://x"}, TLSPin: "nothex"}, nil); err == nil {
		t.Fatal("want error for an invalid tls_pin")
	}
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(v)
}
