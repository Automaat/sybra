package sybra

import (
	"fmt"
	"net/url"
)

// BrowserService opens external URLs in an in-app webview window so the user can
// browse GitHub — and log in — without leaving Sybra. The window shares the
// app's persistent cookie store, so a session established here survives both
// navigation and app restarts.
//
// Window creation lives in the darwin-only Wails layer, so it is injected as a
// closure (open) from the desktop entrypoint rather than imported here — this
// keeps the service compiling on the headless server build, where open is nil
// and Open reports the feature as unavailable.
type BrowserService struct {
	open func(string) // set in wireServices from App.openBrowser; nil on server
}

// Open loads rawURL in a new in-app browser window. Only http(s) URLs are
// accepted; anything else (e.g. file://, javascript:) is rejected so a crafted
// task field can't drive the embedded webview at a local resource.
func (s *BrowserService) Open(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url has no host")
	}
	if s.open == nil {
		return fmt.Errorf("in-app browser is unavailable on this build")
	}
	s.open(u.String())
	return nil
}
