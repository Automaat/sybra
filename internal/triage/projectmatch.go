package triage

import (
	"regexp"
	"strings"

	"github.com/Automaat/sybra/internal/project"
)

// githubURLRe matches a GitHub repo URL and captures owner/repo.
// Tolerates trailing paths (/pull/123, /issues/42, .git, etc.) and punctuation.
var githubURLRe = regexp.MustCompile(`https?://github\.com/([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)`)

// MatchProject looks at the task's title+body, extracts any github.com URL,
// and returns the matching registered project's ID ("owner/repo"). Returns ""
// if no URL is present or no matching project is registered.
func MatchProject(title, body string, projects []project.Project) string {
	text := title + "\n" + body
	matches := githubURLRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		owner, repo := m[1], strings.TrimSuffix(m[2], ".git")
		id := owner + "/" + repo
		for i := range projects {
			if projects[i].ID == id {
				return projects[i].ID
			}
			if projects[i].Owner == owner && projects[i].Repo == repo {
				return projects[i].ID
			}
		}
	}
	return ""
}
