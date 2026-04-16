package main

import (
	"context"
	"embed"
	"log"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/sybra"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		println("Error loading config:", err.Error())
		return
	}

	logger, levelVar, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		println("Error initializing logger:", err.Error())
		return
	}
	defer cleanup()

	// Route Go's default log (used by net/http for idle channel noise)
	// through slog at DEBUG so it doesn't pollute stderr.
	log.SetFlags(0)
	log.SetOutput(slogWriter{logger})

	startPprof(logger)

	app := sybra.NewApp(logger, levelVar, cfg,
		sybra.WithEmitFactory(func(ctx context.Context) func(string, any) {
			return func(event string, data any) {
				wailsruntime.EventsEmit(ctx, event, data)
			}
		}),
	)

	var (
		quitArmed bool
		quitMu    sync.Mutex
		quitTimer *time.Timer
	)

	appMenu := menu.NewMenu()
	appMenu.Append(menu.EditMenu())
	appMenu.Append(menu.WindowMenu())
	fileMenu := appMenu.AddSubmenu("File")
	fileMenu.AddText("Close Window", keys.CmdOrCtrl("w"), func(_ *menu.CallbackData) {
		wailsruntime.Quit(app.Context())
	})
	fileMenu.AddText("Quit", keys.CmdOrCtrl("q"), func(_ *menu.CallbackData) {
		quitMu.Lock()
		defer quitMu.Unlock()

		if quitArmed {
			wailsruntime.Quit(app.Context())
			return
		}

		quitArmed = true
		wailsruntime.EventsEmit(app.Context(), events.AppQuitConfirm)
		quitTimer = time.AfterFunc(3*time.Second, func() {
			quitMu.Lock()
			defer quitMu.Unlock()
			quitArmed = false
		})
		_ = quitTimer
	})

	err = wails.Run(&options.App{
		Title:            "Sybra",
		Width:            1280,
		Height:           800,
		WindowStartState: options.Maximised,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		Mac: &mac.Options{
			Preferences: &mac.Preferences{
				FullscreenEnabled: mac.Enabled,
			},
		},
		OnStartup: func(ctx context.Context) {
			if err := app.Startup(ctx); err != nil {
				logger.Error("app.startup.fatal", "err", err)
				wailsruntime.Quit(ctx)
			}
		},
		OnShutdown: app.Shutdown,
		Menu:       appMenu,
		Bind:       app.BindTargets(),
	})

	if err != nil {
		logger.Error("app.fatal", "err", err)
		println("Error:", err.Error())
	}
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
