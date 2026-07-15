package sybra

import "testing"

func TestBrowserServiceOpen(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantOpen string // URL passed to the opener; "" means opener must not be called
		wantErr  bool
	}{
		{name: "https github", url: "https://github.com/o/r/issues/1", wantOpen: "https://github.com/o/r/issues/1"},
		{name: "http allowed", url: "http://example.com", wantOpen: "http://example.com"},
		{name: "file scheme rejected", url: "file:///etc/passwd", wantErr: true},
		{name: "javascript scheme rejected", url: "javascript:alert(1)", wantErr: true},
		{name: "no scheme rejected", url: "github.com/o/r", wantErr: true},
		{name: "empty host rejected", url: "https://", wantErr: true},
		{name: "unparseable rejected", url: "https://exa mple.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			s := &BrowserService{open: func(u string) { got = u }}

			err := s.Open(tt.url)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Open(%q) = nil error, want error", tt.url)
				}
				if got != "" {
					t.Fatalf("Open(%q) called opener with %q on a rejected URL", tt.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q) unexpected error: %v", tt.url, err)
			}
			if got != tt.wantOpen {
				t.Fatalf("Open(%q) opened %q, want %q", tt.url, got, tt.wantOpen)
			}
		})
	}
}

func TestBrowserServiceOpenUnavailable(t *testing.T) {
	// On the server build the opener is never injected; a valid URL must error
	// rather than panic on the nil closure.
	s := &BrowserService{}
	if err := s.Open("https://github.com"); err == nil {
		t.Fatal("Open with nil opener = nil error, want unavailable error")
	}
}
