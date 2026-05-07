// Command sybra-v3 is the Phase 2/3 swap-the-bootstrap target of the Wails
// v2 → v3 migration: it reuses the existing sybra.App + 12 services on the
// v3 runtime instead of porting each service to a parallel-track package.
//
// Not on main. Not for production use until v3 hits beta and Phase 4
// (frontend bindings regen) lands. See docs/migrations/wails-v3.md.
//
// Darwin-only because Wails v3 alpha needs gtk3/webkit2gtk-4.1 system
// headers on Linux. main_other.go provides the no-op linux stub so
// `go build ./...` and govulncheck stay green.

//go:build darwin

package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"log/slog"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/sybra"
)

//go:embed all:dist
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

	// emit is late-bound: sybra.App's WithEmitFactory captures a closure
	// that delegates to v3emit, which is rebound after application.New().
	// Events fired during App.Startup (before v3 app creation) are dropped —
	// acceptable for the spike; revisit when Phase 4 rebuilds the frontend.
	v3emit := func(string, any) {}

	sybraApp := sybra.NewApp(logger, levelVar, cfg,
		sybra.WithEmitFactory(func(_ context.Context) func(string, any) {
			return func(event string, data any) { v3emit(event, data) }
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sybraApp.Startup(ctx); err != nil {
		logger.Error("sybra.startup.fatal", "err", err)
		return fmt.Errorf("sybra startup: %w", err)
	}
	defer sybraApp.Shutdown(ctx)

	v3app := application.New(application.Options{
		Name:        "Sybra (v3)",
		Description: "Sybra desktop app on Wails v3 alpha",
		LogLevel:    slog.LevelInfo,
		Services:    sybraApp.V3Services(),
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	v3emit = func(event string, data any) {
		v3app.Event.Emit(event, data)
	}

	v3app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Sybra (v3)",
		Width:  1280,
		Height: 800,
	})

	return v3app.Run()
}
