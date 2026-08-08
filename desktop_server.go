//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
)

// desktopServerHost binds the window's board to loopback only. The window is
// the sole client, and the bearer token that reaches it is written into a
// response only this interface can ask for.
const desktopServerHost = "127.0.0.1"

const desktopReadHeaderTimeout = 5 * time.Second

const desktopShutdownDeadline = 5 * time.Second

// desktopAssetRoot is where the embedded bundle sits inside the binary.
const desktopAssetRoot = "frontend/dist"

// desktopPortFile records the port the window was last served on.
//
// Browser storage is partitioned by origin, port included, so a fresh
// ephemeral port every launch silently empties localStorage — the colour
// scheme, the open workspace tabs, the pane sizes, and every other UI
// preference reset on each start, and on each auto-update restart.
const desktopPortFile = "desktop-port"

// desktopBoard is the origin the window loads the UI from.
type desktopBoard struct {
	url string
	srv *http.Server
}

// openDesktopBoard serves the window this process's own board.
//
// The window runs the same bundle a browser gets from sybra-server and reaches
// state over the same HTTP and SSE endpoints, so there is one transport to keep
// working rather than two that drift.
func openDesktopBoard(ctx context.Context, cfg *config.Config, logger *slog.Logger, broker *sse.Broker, app *sybra.App) (*desktopBoard, error) {
	sub, err := desktopAssets()
	if err != nil {
		return nil, err
	}
	ln, err := listenDesktop(ctx, logger)
	if err != nil {
		return nil, err
	}
	origin := "http://" + ln.Addr().String()

	return serveDesktopBoard(ln, origin, logger, cfg, httpserve.Options{
		Logger:      logger,
		Broker:      broker,
		Services:    sybra.ServiceRegistry(app),
		Admit:       app.HTTPAdmission,
		StaticFS:    sub,
		EnablePprof: httpserve.PprofEnabled(),
		APIBase:     origin + "/api",
		Token:       cfg.Server.AuthToken,
		SelfOrigin:  origin,
	})
}

// openAttachedBoard serves the UI for a board on another machine.
//
// Only the bundle and the runtime config come from here; every call and every
// event goes to the other board. BrowserService stays local because opening a
// window or a link is an action on the machine the operator is sitting at, and
// the remote board refuses it anyway.
func openAttachedBoard(ctx context.Context, cfg *config.Config, logger *slog.Logger, remote remoteTarget, openBrowser func(string)) (*desktopBoard, error) {
	sub, err := desktopAssets()
	if err != nil {
		return nil, err
	}
	ln, err := listenDesktop(ctx, logger)
	if err != nil {
		return nil, err
	}
	origin := "http://" + ln.Addr().String()
	logger.Info("desktop.board.attached", "origin", remote.origin)

	return serveDesktopBoard(ln, origin, logger, cfg, httpserve.Options{
		Logger:        logger,
		Services:      sybra.LocalBrowserServices(openBrowser),
		StaticFS:      sub,
		APIBase:       remote.origin + "/api",
		Token:         remote.token,
		SelfOrigin:    origin,
		ConnectOrigin: remote.origin,
	})
}

func desktopAssets() (fs.FS, error) {
	sub, err := fs.Sub(assets, desktopAssetRoot)
	if err != nil {
		return nil, fmt.Errorf("desktop assets: %w", err)
	}
	return sub, nil
}

func serveDesktopBoard(ln net.Listener, origin string, logger *slog.Logger, cfg *config.Config, opts httpserve.Options) (*desktopBoard, error) {
	handler := httpserve.Handler(opts, cfg.Server.AuthToken, cfg.Server.AllowedOrigins)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: desktopReadHeaderTimeout}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("desktop.board.serve", "err", err)
		}
	}()
	logger.Info("desktop.board.listen", "origin", origin)
	return &desktopBoard{url: origin + "/", srv: srv}, nil
}

// shutdown bounds the wait. http.Server.Shutdown never cancels a handler, and
// a live /events stream only returns when its request context is done, so an
// unbounded wait here hangs quit for as long as one window holds the stream.
func (b *desktopBoard) shutdown(ctx context.Context) {
	if b == nil || b.srv == nil {
		return
	}
	// WithoutCancel: the run context is usually already on its way down by the
	// time this runs, and a cancelled parent would skip the drain entirely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopShutdownDeadline)
	defer cancel()
	if err := b.srv.Shutdown(ctx); err != nil {
		_ = b.srv.Close()
	}
}

// listenDesktop reuses the port the window was last served on, so the origin —
// and with it every localStorage-backed preference — survives a restart. A port
// someone else took falls back to an ephemeral one, which costs the stored
// preferences that once but keeps the app starting.
func listenDesktop(ctx context.Context, logger *slog.Logger) (net.Listener, error) {
	var lc net.ListenConfig
	if port := readDesktopPort(); port != "" {
		ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(desktopServerHost, port))
		if err == nil {
			return ln, nil
		}
		logger.Warn("desktop.board.port.taken", "port", port, "err", err)
	}
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(desktopServerHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", desktopServerHost, err)
	}
	if _, port, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
		writeDesktopPort(logger, port)
	}
	return ln, nil
}

func desktopPortPath() string {
	return filepath.Join(config.HomeDir(), desktopPortFile)
}

func readDesktopPort() string {
	data, err := os.ReadFile(desktopPortPath())
	if err != nil {
		return ""
	}
	port := strings.TrimSpace(string(data))
	n, err := strconv.Atoi(port)
	if err != nil || n < 1024 || n > 65535 {
		return ""
	}
	return port
}

func writeDesktopPort(logger *slog.Logger, port string) {
	if err := os.WriteFile(desktopPortPath(), []byte(port+"\n"), 0o600); err != nil {
		logger.Warn("desktop.board.port.persist", "err", err)
	}
}

// remoteTarget is a board on another machine this window was pointed at.
type remoteTarget struct {
	origin string
	token  string
}

// remoteBoard resolves SYBRA_SERVER_TARGET, accepting every form sybra-cli
// accepts so an operator's existing export means the same thing to both.
//
// A value that cannot be resolved is an error, never a silent fall-through to
// this machine's board: that is exactly how someone ends up editing their
// laptop while believing they are looking at the server.
func remoteBoard() (target remoteTarget, attached bool, err error) {
	raw := strings.TrimSpace(os.Getenv("SYBRA_SERVER_TARGET"))
	if raw == "" {
		return remoteTarget{}, false, nil
	}
	origin, err := remoteOrigin(raw)
	if err != nil {
		return remoteTarget{}, false, err
	}
	token := strings.TrimSpace(os.Getenv("SYBRA_SERVER_TOKEN"))
	if token == "" {
		return remoteTarget{}, false, fmt.Errorf("SYBRA_SERVER_TARGET=%q requires SYBRA_SERVER_TOKEN: that board's token is not in this machine's config", raw)
	}
	return remoteTarget{origin: origin, token: token}, true, nil
}

func remoteOrigin(raw string) (string, error) {
	target := raw
	scheme := "http"
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "", fmt.Errorf("SYBRA_SERVER_TARGET=%q is not an http(s) origin or host:port", raw)
		}
		if path := strings.TrimSpace(u.EscapedPath()); path != "" && path != "/" {
			return "", fmt.Errorf("SYBRA_SERVER_TARGET=%q must carry no path", raw)
		}
		scheme, target = u.Scheme, u.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", fmt.Errorf("SYBRA_SERVER_TARGET=%q is not an http(s) origin or host:port", raw)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("SYBRA_SERVER_TARGET=%q has no valid port", raw)
	}
	if strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("SYBRA_SERVER_TARGET=%q has no host", raw)
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}
