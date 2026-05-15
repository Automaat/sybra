package main

import (
	"net/http"
	"testing"
)

func TestFormatAPIError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
		want   string
	}{
		{
			name:   "valid envelope",
			status: http.StatusNotFound,
			body:   []byte(`{"error":"unknown service: NoSvc","code":"not_found"}`),
			want:   "unknown service: NoSvc [not_found] (HTTP 404)",
		},
		{
			name:   "valid envelope internal error",
			status: http.StatusInternalServerError,
			body:   []byte(`{"error":"internal error","code":"internal_error"}`),
			want:   "internal error [internal_error] (HTTP 500)",
		},
		{
			name:   "malformed JSON",
			status: http.StatusBadRequest,
			body:   []byte(`not json`),
			want:   "not json (HTTP 400)",
		},
		{
			name:   "empty body",
			status: http.StatusServiceUnavailable,
			body:   []byte{},
			want:   "Service Unavailable (HTTP 503)",
		},
		{
			name:   "non-JSON text",
			status: http.StatusInternalServerError,
			body:   []byte("something went wrong"),
			want:   "something went wrong (HTTP 500)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAPIError(tc.status, tc.body)
			if got != tc.want {
				t.Fatalf("formatAPIError(%d, %q) = %q; want %q", tc.status, tc.body, got, tc.want)
			}
		})
	}
}
