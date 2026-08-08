//go:build darwin

package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
)

// desktopServerAddr binds the window's board to loopback only. The window is
// the sole client, and the bearer token that reaches it is written into a
// response only this interface can ask for.
const desktopServerAddr = "127.0.0.1:0"

const desktopReadHeaderTimeout = 5 * time.Second

// openDesktopBoard resolves the board this window attaches to: the operator's
// remote target when one is named, otherwise this process's own.
func openDesktopBoard(ctx context.Context, cfg *config.Config, logger *slog.Logger, broker *sse.Broker, app *sybra.App, assets fs.FS) (*desktopBoard, error) {
	if remote, ok := remoteBoardURL(); ok {
		logger.Info("desktop.board.remote", "url", remote)
		return &desktopBoard{url: remote}, nil
	}
	sub, err := fs.Sub(assets, desktopAssetRoot)
	if err != nil {
		return nil, fmt.Errorf("desktop assets: %w", err)
	}
	return serveDesktopBoard(ctx, cfg, logger, broker, app, sub)
}

// desktopAssetRoot is where the embedded bundle sits inside the binary.
const desktopAssetRoot = "frontend/dist"

// desktopBoard is the origin the window loads the UI from.
type desktopBoard struct {
	// url is where the window points. Local boards carry the port the
	// in-process listener picked; a remote board carries the operator's target.
	url string
	// srv is nil for a remote board, which serves its own UI.
	srv *http.Server
}

// serveDesktopBoard starts the in-process board the desktop window talks to.
//
// The window runs the same bundle a browser gets from sybra-server and reaches
// state over the same HTTP and SSE endpoints, so there is one transport to keep
// working rather than two that drift.
func serveDesktopBoard(ctx context.Context, cfg *config.Config, logger *slog.Logger, broker *sse.Broker, app *sybra.App, assets fs.FS) (*desktopBoard, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", desktopServerAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", desktopServerAddr, err)
	}

	origin := "http://" + ln.Addr().String()
	opts := httpserve.Options{
		Logger:      logger,
		Broker:      broker,
		Services:    sybra.ServiceRegistry(app),
		Admit:       app.HTTPAdmission,
		StaticFS:    assets,
		EnablePprof: httpserve.PprofEnabled(),
		APIBase:     origin + "/api",
		Token:       cfg.Server.AuthToken,
	}
	handler := httpserve.Handler(opts, cfg.Server.AuthToken, cfg.Server.AllowedOrigins)

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: desktopReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !strings.Contains(err.Error(), "Server closed") {
			logger.Error("desktop.board.serve", "err", err)
		}
	}()
	logger.Info("desktop.board.listen", "origin", origin)
	return &desktopBoard{url: origin + "/", srv: srv}, nil
}

func (b *desktopBoard) shutdown(ctx context.Context) {
	if b == nil || b.srv == nil {
		return
	}
	_ = b.srv.Shutdown(ctx)
}

// remoteBoardURL reports the board on another machine this window was pointed
// at, using the same environment contract sybra-cli reads. That board serves
// its own UI, so the window loads it directly and the local listener is never
// started.
func remoteBoardURL() (string, bool) {
	raw := strings.TrimSpace(os.Getenv("SYBRA_SERVER_TARGET"))
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return u.Scheme + "://" + u.Host + "/", true
}
