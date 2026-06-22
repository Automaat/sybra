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
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/sybra"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger, levelVar, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		return fmt.Errorf("logging: %w", err)
	}
	defer cleanup()

	log.SetFlags(0)
	log.SetOutput(slogWriter{logger})

	startPprof(logger)

	v3emit := func(string, any) {}

	sybraApp := sybra.NewApp(logger, levelVar, cfg,
		sybra.WithSkillsFS(skills.FS),
		sybra.WithEmitFactory(func(_ context.Context) func(string, any) {
			return func(event string, data any) { v3emit(event, data) }
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sybraApp.Startup(ctx); err != nil {
		logger.Error("app.startup.fatal", "err", err)
		return fmt.Errorf("sybra startup: %w", err)
	}
	defer sybraApp.Shutdown(ctx)

	v3app := application.New(application.Options{
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
	desktopEvents.onStall = func(d time.Duration) { go sybraApp.NotifyUIStall(d) }
	desktopEvents.onRecovered = func(d time.Duration) { go sybraApp.NotifyUIRecovered(d) }
	v3emit = desktopEvents.Emit

	v3app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Sybra",
		Width:            1280,
		Height:           800,
		StartState:       application.WindowStateMaximised,
		BackgroundColour: application.RGBA{Red: 27, Green: 38, Blue: 54, Alpha: 1},
	})

	return v3app.Run()
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
