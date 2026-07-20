package config

// BrowserConfig controls how the desktop app opens external links (GitHub,
// PRs, issues). Read once at startup — flipping this value requires an app
// restart, since the opener closure is wired into the app at boot in main.go.
type BrowserConfig struct {
	// InApp opens links in an in-app Sybra Browser webview window backed by a
	// persistent, app-wide cookie store, so a login is reused across windows
	// and survives restarts. Nil/unset defaults to false, so the built-in
	// browser stays opt-in. Set to true to use the Sybra browser instead of the
	// default system browser.
	InApp *bool `yaml:"in_app" json:"inApp"`
}
