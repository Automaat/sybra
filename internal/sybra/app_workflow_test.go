package sybra

import "testing"

func TestEligibleRerequestReviewer(t *testing.T) {
	tests := []struct {
		name     string
		login    string
		viewer   string
		author   string
		expected bool
	}{
		{name: "comment author", login: "alice", viewer: "me", author: "author", expected: true},
		{name: "empty", login: "", viewer: "me", author: "author", expected: false},
		{name: "viewer", login: "me", viewer: "me", author: "author", expected: false},
		{name: "pr author", login: "author", viewer: "me", author: "author", expected: false},
		{name: "bot", login: "renovate[bot]", viewer: "me", author: "author", expected: false},
		{name: "case-insensitive viewer", login: "Me", viewer: "me", author: "author", expected: false},
		{name: "case-insensitive author", login: "Author", viewer: "me", author: "author", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eligibleRerequestReviewer(tt.login, tt.viewer, tt.author)
			if got != tt.expected {
				t.Fatalf("eligibleRerequestReviewer(%q, %q, %q) = %v, want %v",
					tt.login, tt.viewer, tt.author, got, tt.expected)
			}
		})
	}
}
