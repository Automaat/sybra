//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoteBoard_AcceptsEveryFormTheCLIAccepts is the guard on a silent wrong
// target. A value the resolver rejects used to fall through to this machine's
// board, so an operator with the CLI's own bare host:port exported would be
// shown their laptop while believing they were looking at the server.
func TestRemoteBoard_AcceptsEveryFormTheCLIAccepts(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		token      string
		wantOrigin string
		wantErr    bool
	}{
		{name: "unset", wantOrigin: ""},
		{name: "bare host port", target: "192.168.20.219:8080", token: "t", wantOrigin: "http://192.168.20.219:8080"},
		{name: "http origin", target: "http://192.168.20.219:8080", token: "t", wantOrigin: "http://192.168.20.219:8080"},
		{name: "https origin", target: "https://board.example:443", token: "t", wantOrigin: "https://board.example:443"},
		{name: "trailing slash", target: "https://board.example:443/", token: "t", wantOrigin: "https://board.example:443"},
		{name: "host without port", target: "board.example", token: "t", wantErr: true},
		{name: "carries a path", target: "https://board.example:443/board", token: "t", wantErr: true},
		{name: "unsupported scheme", target: "ftp://board.example:21", token: "t", wantErr: true},
		{name: "port out of range", target: "board.example:70000", token: "t", wantErr: true},
		{name: "no token", target: "192.168.20.219:8080", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SYBRA_SERVER_TARGET", tc.target)
			t.Setenv("SYBRA_SERVER_TOKEN", tc.token)

			target, attached, err := remoteBoard()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("remoteBoard() = %+v, %v, nil; want an error rather than a fall back to this machine's board", target, attached)
				}
				if attached {
					t.Fatal("attached = true alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("remoteBoard(): %v", err)
			}
			if attached != (tc.wantOrigin != "") {
				t.Fatalf("attached = %v, want %v", attached, tc.wantOrigin != "")
			}
			if target.origin != tc.wantOrigin {
				t.Fatalf("origin = %q, want %q", target.origin, tc.wantOrigin)
			}
		})
	}
}

// TestDesktopPort_SurvivesARestart pins the origin the window loads from.
// Browser storage is partitioned by port, so a port that changes per launch
// empties localStorage and resets every UI preference on each start.
func TestDesktopPort_SurvivesARestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	if got := readDesktopPort(); got != "" {
		t.Fatalf("readDesktopPort() = %q on a fresh home, want empty", got)
	}
	writeDesktopPort(testLogger(), "51234")
	if got := readDesktopPort(); got != "51234" {
		t.Fatalf("readDesktopPort() = %q, want 51234", got)
	}

	// A junk or privileged value is ignored rather than trusted, so a corrupted
	// file costs one origin instead of failing every launch.
	for _, bad := range []string{"", "not-a-port", "80", "70000"} {
		if err := os.WriteFile(filepath.Join(home, desktopPortFile), []byte(bad), 0o600); err != nil {
			t.Fatalf("write port file: %v", err)
		}
		if got := readDesktopPort(); got != "" {
			t.Fatalf("readDesktopPort() = %q for %q, want empty", got, bad)
		}
	}
}
