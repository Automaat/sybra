package umbrella

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
)

// TrackedFilesFunc resolves the set of file paths currently tracked in a
// repo's default branch. Injected so grounding never network-fetches on the
// hot expansion path and stays unit-testable without a real repo.
type TrackedFilesFunc func(ctx context.Context, repo string) ([]string, error)

// groundReport summarizes a Plan.ground run: which repos were successfully
// grounded, and which repos/refs were skipped (fail-open — a lister error or
// an unregistered repo never aborts expansion, it just leaves that child's
// touches as the planner reported them).
type groundReport struct {
	groundedRepos []string
	skipped       []string
}

var (
	backtickSpanRe = regexp.MustCompile("`([^`]+)`")
	// pathTokenRe matches slash-delimited, path-shaped tokens in plain text.
	// A path shape requires at least one '/' so bare words are never treated
	// as paths.
	pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+`)
	// issueRefRe matches a full "owner/repo#n" issue/PR reference so it is
	// never mistaken for a repo path.
	issueRefRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+#\d+$`)
	// urlRe matches a whole URL so it is stripped from plain text before
	// path-token scanning — otherwise a URL's path segment (e.g.
	// "github.com/o/r/blob/main/internal/foo.go") would be mistaken for a
	// repo-relative path.
	urlRe = regexp.MustCompile(`\S+://\S+`)
)

// extractPaths pulls candidate repo-relative paths out of a sub-issue body:
// backtick code spans (verbatim, including spaces) plus bare path-shaped
// tokens in the surrounding text. It skips `://` URLs and issue references
// (`owner/repo#n` or bare `#n`) so those never get mistaken for a path, and
// strips trailing markdown punctuation. Order-preserving, de-duplicated
// case-insensitively.
func extractPaths(body string) []string {
	var tokens []string
	seen := make(map[string]bool)
	add := func(tok string) {
		tok = strings.TrimSpace(tok)
		tok = strings.Trim(tok, ",.():;!?'\"")
		if tok == "" || !strings.Contains(tok, "/") {
			return
		}
		if strings.Contains(tok, "://") {
			return
		}
		if strings.HasPrefix(tok, "#") || issueRefRe.MatchString(tok) {
			return
		}
		key := strings.ToLower(tok)
		if seen[key] {
			return
		}
		seen[key] = true
		tokens = append(tokens, tok)
	}

	for _, m := range backtickSpanRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	plain := backtickSpanRe.ReplaceAllString(body, " ")
	plain = urlRe.ReplaceAllString(plain, " ")
	for _, m := range pathTokenRe.FindAllString(plain, -1) {
		add(m)
	}
	return tokens
}

// fileSet indexes a repo's tracked files for confirming candidate paths:
// exact tracked files plus every ancestor directory of each, so a directory
// mention (e.g. "internal/foo") confirms without requiring the caller to name
// a specific file inside it.
type fileSet struct {
	files map[string]bool
	dirs  map[string]bool
}

// newFileSet builds a fileSet from a repo's tracked file list.
func newFileSet(tracked []string) fileSet {
	fs := fileSet{files: make(map[string]bool, len(tracked)), dirs: make(map[string]bool)}
	for _, f := range tracked {
		n := normalizePath(f)
		if n == "" {
			continue
		}
		fs.files[n] = true
		segs := strings.Split(n, "/")
		for i := 1; i < len(segs); i++ {
			fs.dirs[strings.Join(segs[:i], "/")] = true
		}
	}
	return fs
}

// confirm reports whether token names a tracked file or a real ancestor
// directory, comparing segment-wise (via normalizePath's exact-string keys)
// so "internal/foo" never confirms against a sibling "internal/foobar/x.go".
// Returns the normalized path on success.
func (fs fileSet) confirm(token string) (string, bool) {
	n := normalizePath(token)
	if n == "" {
		return "", false
	}
	if fs.files[n] || fs.dirs[n] {
		return n, true
	}
	return "", false
}

// groundTouches extracts candidate paths from body and returns the ones
// confirmed against fs, normalized and de-duplicated, order-preserving.
func groundTouches(fs fileSet, body string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, tok := range extractPaths(body) {
		n, ok := fs.confirm(tok)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ground confirms each child's touches against its repo's real tracked files
// and unions any newly-confirmed paths in, in place. Gated once on len(subs)
// vs minSubs (minSubs <= 0 means always ground). Per child, an unresolvable
// ref or a lister failure records a skip and moves on — grounding never
// aborts expansion. The tracked-file lookup is memoized per repo so a shared
// repo across children only calls lister once.
func (p *Plan) ground(ctx context.Context, lister TrackedFilesFunc, subs []SubIssue, minSubs int) groundReport {
	var report groundReport
	if lister == nil {
		return report
	}
	if minSubs > 0 && len(subs) < minSubs {
		return report
	}

	bodyByRef := make(map[string]string, len(subs))
	for _, s := range subs {
		bodyByRef[NormalizeIssueRef(s.Ref)] = s.Body
	}

	type cacheEntry struct {
		fs fileSet
		ok bool
	}
	cache := make(map[string]cacheEntry)
	groundedRepos := make(map[string]bool)

	for i := range p.Children {
		c := &p.Children[i]
		repo, _, ok := ParseRef(c.Ref)
		if !ok {
			report.skipped = append(report.skipped, c.Ref)
			continue
		}

		entry, cached := cache[repo]
		if !cached {
			files, err := lister(ctx, repo)
			if err != nil {
				slog.DebugContext(ctx, "umbrella.ground.skip", "repo", repo, "error", err)
				cache[repo] = cacheEntry{}
				report.skipped = append(report.skipped, repo)
				continue
			}
			entry = cacheEntry{fs: newFileSet(files), ok: true}
			cache[repo] = entry
			groundedRepos[repo] = true
		}
		if !entry.ok {
			report.skipped = append(report.skipped, repo)
			continue
		}

		grounded := groundTouches(entry.fs, bodyByRef[NormalizeIssueRef(c.Ref)])
		if len(grounded) == 0 {
			continue
		}
		c.Touches = unionTouches(c.Touches, grounded)
	}

	repos := make([]string, 0, len(groundedRepos))
	for r := range groundedRepos {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	report.groundedRepos = repos
	return report
}

// unionTouches merges add into existing, de-duplicated by normalizePath,
// keeping the existing entries' original spelling first.
func unionTouches(existing, add []string) []string {
	seen := make(map[string]bool, len(existing)+len(add))
	out := make([]string, 0, len(existing)+len(add))
	for _, e := range existing {
		n := normalizePath(e)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, e)
	}
	for _, a := range add {
		n := normalizePath(a)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, a)
	}
	return out
}
