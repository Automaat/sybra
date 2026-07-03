// Sybra desktop entry point — Wails v3.
//
// Phase 5 of the v2→v3 migration. The repo-root main.go now boots the v3
// runtime directly; the parallel-track cmd/sybra-v3 binary that drove the
// migration is gone. See docs/migrations/wails-v3.md.
//
// Darwin-only because Wails v3 alpha needs gtk3/webkit2gtk-4.1 system
// headers on Linux that the CI runners do not have. main_other.go is the
// no-op stub for non-darwin so `go build ./...` and govulncheck stay green.

//go:build darwin

package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Automaat/sybra/internal/autoupdate"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/sybra"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	code, err := run()
	if err != nil {
		log.Print(err)
		if code == 0 {
			code = 1
		}
	}
	if code != 0 {
		os.Exit(code)
	}
}

func run() (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 1, fmt.Errorf("config: %w", err)
	}

	logger, levelVar, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		return 1, fmt.Errorf("logging: %w", err)
	}
	defer cleanup()

	log.SetFlags(0)
	log.SetOutput(slogWriter{logger})

	startPprof(logger)

	logger.Info("browser.in_app", "enabled", cfg.InAppBrowserEnabled())

	v3emit := func(string, any) {}
	v3openBrowser := func(string) {}
	var restartRequested atomic.Bool
	var v3app *application.App

	opts := buildAppOptions(cfg, logger, &v3emit, &v3openBrowser, &restartRequested, &v3app)
	sybraApp := sybra.NewApp(logger, levelVar, cfg, opts...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v3app = application.New(application.Options{
		Name:        "Sybra",
		Description: "Sybra orchestrator",
		LogLevel:    slog.LevelInfo,
		Services:    sybraApp.V3Services(),
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	desktopEvents := newDesktopEmitter(ctx, logger, func(event string, data any) {
		v3app.Event.Emit(event, data)
	})
	// Surface a wedged UI thread via an OS-native notification (osascript),
	// which does not depend on the frozen webview. Run async so the emit pump
	// never blocks on the alert. The backend keeps running; only the window is
	// frozen, so restart is the recovery — Wails main-thread APIs (Reload)
	// route through the same blocked path and cannot self-heal.
	desktopEvents.onStall = func(d time.Duration) {
		go func() {
			_ = notification.SendDesktop("Sybra UI stalled",
				fmt.Sprintf("No UI updates for %s — the window is likely frozen. Restart Sybra to recover; the backend keeps running.", d.Round(time.Second)))
		}()
	}
	desktopEvents.onRecovered = func(d time.Duration) {
		go func() {
			_ = notification.SendDesktop("Sybra UI recovered",
				fmt.Sprintf("UI updates resumed after %s.", d.Round(time.Second)))
		}()
	}
	v3emit = desktopEvents.Emit
	v3openBrowser = func(url string) { openInAppBrowser(v3app, url) }

	if err := sybraApp.Startup(ctx); err != nil {
		logger.Error("app.startup.fatal", "err", err)
		return 1, fmt.Errorf("sybra startup: %w", err)
	}
	defer sybraApp.Shutdown(ctx)
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}

	v3app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Sybra",
		Width:            1280,
		Height:           800,
		StartState:       application.WindowStateMaximised,
		BackgroundColour: application.RGBA{Red: 27, Green: 38, Blue: 54, Alpha: 1},
	})

	if err := v3app.Run(); err != nil {
		return 1, err
	}
	if restartRequested.Load() {
		return autoupdate.RestartExitCode, nil
	}
	return 0, nil
}

// buildAppOptions assembles the sybra.Option set for the desktop app.
// v3emit/v3openBrowser/v3app are pointers to run's locals so the restart and
// browser-opener callbacks can reach state that isn't set up until later in
// startup (the v3app application.App and the wired emit/opener funcs).
func buildAppOptions(
	cfg *config.Config,
	logger *slog.Logger,
	v3emit *func(string, any),
	v3openBrowser *func(string),
	restartRequested *atomic.Bool,
	v3app **application.App,
) []sybra.Option {
	opts := []sybra.Option{
		sybra.WithSkillsFS(skills.FS),
		sybra.WithEmitFactory(func(_ context.Context) func(string, any) {
			return func(event string, data any) { (*v3emit)(event, data) }
		}),
	}
	opts = append(opts, desktopBrowserOptions(cfg, func(url string) { (*v3openBrowser)(url) })...)
	opts = append(opts, sybra.WithRestartRequest(func() {
		homeDir := config.HomeDir()
		if err := autoupdate.WriteRestartMarker(homeDir); err != nil {
			logger.Error("autoupdate.restart.marker.failed", "err", err)
		} else {
			logger.Info("autoupdate.restart.marker.written", "path", autoupdate.RestartMarkerPath(homeDir))
		}
		restartRequested.Store(true)
		if *v3app != nil {
			(*v3app).Quit()
		}
	}))
	return opts
}

// desktopBrowserOptions returns the sybra.Option(s) that wire the in-app
// browser opener, or none when the config toggle disables it. When omitted,
// BrowserService.Open keeps its nil-opener "unavailable" behavior, and the
// frontend's existing catch handler falls back to the system browser.
func desktopBrowserOptions(cfg *config.Config, opener func(string)) []sybra.Option {
	if !cfg.InAppBrowserEnabled() {
		return nil
	}
	return []sybra.Option{sybra.WithBrowserOpener(opener)}
}

// openInAppBrowser opens url in a fresh in-app webview window. The window uses
// the app's default (persistent, app-wide) WKWebsiteDataStore, so a GitHub
// login here is reused across windows and survives restarts — the user logs in
// once and stays in a single app. One window per call by design.
func openInAppBrowser(app *application.App, url string) {
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Sybra Browser",
		URL:              url,
		Width:            1100,
		Height:           850,
		MinWidth:         480,
		MinHeight:        360,
		BackgroundColour: application.RGBA{Red: 255, Green: 255, Blue: 255, Alpha: 1},
	})
}

// startPprof launches a pprof HTTP server when SYBRA_PPROF is set.
// Value "1"/"true" uses 127.0.0.1:6060; any other value is used as-is (host:port).
func startPprof(logger *slog.Logger) {
	addr := os.Getenv("SYBRA_PPROF")
	if addr == "" {
		return
	}
	if addr == "1" || addr == "true" {
		addr = "127.0.0.1:6060"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	go func() {
		logger.Info("pprof.listen", "addr", addr)
		srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		if err := srv.ListenAndServe(); err != nil {
			logger.Error("pprof.serve", "err", err)
		}
	}()
}

// slogWriter routes Go's default log.Print output through slog at DEBUG level.
type slogWriter struct{ logger *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("stdlib.log", "msg", string(p))
	return len(p), nil
}
