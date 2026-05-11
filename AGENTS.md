# Agent Instructions

Codex: read [CLAUDE.md](./CLAUDE.md) before making changes in this repository. Follow the guidance there as the primary repo-specific instruction set.

## Hard Rule: No Work-Data Leak

Sybra is a public personal project. Never link, embed, paraphrase, or otherwise leak content from work repos (e.g. Kong / `konghq.*`) into the sybra repo or any artifact it produces — issues, PRs, task bodies, plan sidecars, commit messages, logs, screenshots. Applies to manual work AND every sybra automation (Todoist, GitHub Issues fetcher, Renovate fixer, orchestrator, reviews).

Forbidden: work-org repo URLs, branch names, commit SHAs from work repos, ticket IDs (Jira keys), internal hostnames, customer names, snippets from work repos. Filter at the source (project-type/source-config), not in post-processing. See **Work-Data Confidentiality** in CLAUDE.md.
