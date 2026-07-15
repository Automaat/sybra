package task

import (
	"strings"
	"testing"
)

// FuzzParseBytes drives random byte streams through the frontmatter parser
// to flush out panics, infinite loops, or unbounded allocations. The parser
// has historically hidden subtle bugs around BOM stripping and YAML edge
// cases — those exact regressions are seeded as the corpus so the fuzzer
// starts from a known-broad shape distribution.
func FuzzParseBytes(f *testing.F) {
	seeds := []string{
		"",
		"---\n---\n",
		"---\nid: a\n---\nbody",
		"---\nid: a\ntitle: t\nstatus: todo\nagent_mode: headless\n---\n# body\n",
		"\xef\xbb\xbf---\nid: a\n---\n",        // BOM + valid
		"---\nid: a\nagent_mode: bogus\n---\n", // invalid mode
		"---\nid: a\nslug: ../escape\n---\n",   // bad slug
		"---\nid: a\nagent_runs:\n- prompt: x\n  result: y\n---\n",
		"---\nid: a\nagent_runs: not-a-list\n---\n",
		"--- \n---\t\n",
		"---\n  \n---\n",
		strings.Repeat("---\n", 10),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// The parser must never panic. Any error return is acceptable.
		_, _ = ParseBytes(data)
	})
}
