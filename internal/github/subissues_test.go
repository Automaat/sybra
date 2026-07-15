package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func umbrellaResponse(umbrella, subNodes string) string {
	return `{"data":{"repository":{"issue":{` + umbrella +
		`,"subIssues":{"nodes":` + subNodes + `}}}}}`
}

func TestFetchUmbrellaWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		output    string
		execErr   error
		wantTitle string
		wantSubs  []int // sub-issue numbers expected, in order
		wantErr   string
	}{
		{
			name: "umbrella with two sub-issues",
			output: umbrellaResponse(
				`"number":100,"title":"umbrella","body":"track","url":"https://github.com/o/r/issues/100","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"}`,
				`[
					{"number":1,"title":"first","body":"b1","url":"https://github.com/o/r/issues/1","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"},"labels":{"nodes":[{"name":"tech-debt"}]}},
					{"number":2,"title":"second","body":"b2","url":"https://github.com/o/r/issues/2","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"},"labels":{"nodes":[]}}
				]`),
			wantTitle: "umbrella",
			wantSubs:  []int{1, 2},
		},
		{
			name: "umbrella with no sub-issues",
			output: umbrellaResponse(
				`"number":100,"title":"empty","body":"","url":"https://github.com/o/r/issues/100","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"}`,
				`[]`),
			wantTitle: "empty",
			wantSubs:  nil,
		},
		{
			name: "truncated sub-issue set is rejected",
			output: `{"data":{"repository":{"issue":{` +
				`"number":100,"title":"big","url":"https://github.com/o/r/issues/100","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"},` +
				`"subIssues":{"totalCount":150,"pageInfo":{"hasNextPage":true},"nodes":[]}}}}}`,
			wantErr: "more than 100 sub-issues",
		},
		{
			name:    "issue not found",
			output:  `{"data":{"repository":{"issue":null}}}`,
			wantErr: "not found",
		},
		{
			name:    "graphql error",
			output:  `{"errors":[{"message":"sub_issues feature unavailable"}]}`,
			wantErr: "graphql: sub_issues feature unavailable",
		},
		{
			name:    "malformed json",
			output:  "not json",
			wantErr: "parse graphql response",
		},
		{
			name:    "exec error",
			output:  "gh: bad credentials",
			execErr: fmt.Errorf("exit 1"),
			wantErr: "gh api graphql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			umb, subs, err := fetchUmbrellaWith(context.Background(), fe, "o/r", 100)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if umb.Title != tt.wantTitle {
				t.Fatalf("umbrella title = %q, want %q", umb.Title, tt.wantTitle)
			}
			if len(subs) != len(tt.wantSubs) {
				t.Fatalf("got %d sub-issues, want %d", len(subs), len(tt.wantSubs))
			}
			for i, n := range tt.wantSubs {
				if subs[i].Number != n {
					t.Fatalf("sub[%d].Number = %d, want %d", i, subs[i].Number, n)
				}
			}
		})
	}
}

// ctxFakeExecer records whether the context-aware path was taken and honors
// context cancellation, so tests can assert FetchUmbrella threads its context.
type ctxFakeExecer struct {
	output  []byte
	usedCtx bool
}

func (c *ctxFakeExecer) run(_ ...string) ([]byte, error) { return c.output, nil }

func (c *ctxFakeExecer) runCtx(ctx context.Context, _ ...string) ([]byte, error) {
	c.usedCtx = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.output, nil
}

func TestFetchUmbrellaWith_UsesContext(t *testing.T) {
	t.Parallel()
	valid := umbrellaResponse(
		`"number":100,"title":"u","body":"","url":"https://github.com/o/r/issues/100","state":"OPEN","repository":{"name":"r","nameWithOwner":"o/r"}`,
		`[]`)

	t.Run("context-aware execer is used", func(t *testing.T) {
		t.Parallel()
		fe := &ctxFakeExecer{output: []byte(valid)}
		if _, _, err := fetchUmbrellaWith(context.Background(), fe, "o/r", 100); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !fe.usedCtx {
			t.Fatal("expected the context-aware runCtx path to be used")
		}
	})

	t.Run("cancelled context aborts the fetch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fe := &ctxFakeExecer{output: []byte(valid)}
		if _, _, err := fetchUmbrellaWith(ctx, fe, "o/r", 100); err == nil {
			t.Fatal("expected an error from a cancelled context")
		}
	})
}

func TestSplitOwnerRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in          string
		owner, name string
		ok          bool
	}{
		{"Automaat/sybra", "Automaat", "sybra", true},
		{"o/r", "o", "r", true},
		{"noSlash", "", "", false},
		{"/r", "", "", false},
		{"o/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		o, n, ok := splitOwnerRepo(c.in)
		if o != c.owner || n != c.name || ok != c.ok {
			t.Errorf("splitOwnerRepo(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, o, n, ok, c.owner, c.name, c.ok)
		}
	}
}
