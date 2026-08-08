package sybra

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Automaat/sybra/internal/sysopen"
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
	u, err := externalURL(rawURL)
	if err != nil {
		return err
	}
	if s.open == nil {
		return unavailableError("in-app browser is unavailable on this build")
	}
	s.open(u)
	return nil
}

// OpenExternal hands rawURL to the browser on the host serving this board.
//
// The UI cannot do this itself: the desktop window's webview implements no
// window-opening delegate, so window.open there is a silent no-op and an
// external link would do nothing at all.
func (s *BrowserService) OpenExternal(ctx context.Context, rawURL string) error {
	u, err := externalURL(rawURL)
	if err != nil {
		return err
	}
	return sysopen.URL(ctx, u)
}

// externalURL accepts only http(s), so a crafted task field cannot drive the
// embedded webview or the host opener at a local resource.
func externalURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", validationError(fmt.Sprintf("parse url: %v", err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", validationError(fmt.Sprintf("unsupported url scheme %q", u.Scheme))
	}
	if u.Host == "" {
		return "", validationError("url has no host")
	}
	return u.String(), nil
}
