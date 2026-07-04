package config

// BrowserConfig controls how the desktop app opens external links (GitHub,
// PRs, issues). Read once at startup — flipping this value requires an app
// restart, since the opener closure is wired into the app at boot in main.go.
type BrowserConfig struct {
	// InApp opens links in an in-app Sybra Browser webview window backed by a
	// persistent, app-wide cookie store, so a login is reused across windows
	// and survives restarts. Nil/unset defaults to true (current behavior).
	// Set to false to always open links in the default system browser instead.
	InApp *bool `yaml:"in_app" json:"inApp"`
}
