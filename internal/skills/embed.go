// Package skills embeds the bundled Claude Code skill definitions so that
// synapse-server can seed ~/.claude/skills/ even when the source repository is
// not present on disk (e.g. Docker deployments).
package skills

import "embed"

// FS contains the bundled .md skill files under data/.
//
//go:embed data/*.md
var FS embed.FS
