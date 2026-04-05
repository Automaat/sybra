package main

import (
	"embed"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/logging"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
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

	app := NewApp(logger, levelVar, cfg)

	var (
		quitArmed bool
		quitMu    sync.Mutex
		quitTimer *time.Timer
	)

	appMenu := menu.NewMenu()
	appMenu.Append(menu.EditMenu())
	fileMenu := appMenu.AddSubmenu("File")
	fileMenu.AddText("Close Window", keys.CmdOrCtrl("w"), func(_ *menu.CallbackData) {
		wailsruntime.Quit(app.ctx)
	})
	fileMenu.AddText("Quit", keys.CmdOrCtrl("q"), func(_ *menu.CallbackData) {
		quitMu.Lock()
		defer quitMu.Unlock()

		if quitArmed {
			wailsruntime.Quit(app.ctx)
			return
		}

		quitArmed = true
		wailsruntime.EventsEmit(app.ctx, events.AppQuitConfirm)
		quitTimer = time.AfterFunc(3*time.Second, func() {
			quitMu.Lock()
			defer quitMu.Unlock()
			quitArmed = false
		})
		_ = quitTimer
	})

	err = wails.Run(&options.App{
		Title:            "Synapse",
		Width:            1280,
		Height:           800,
		WindowStartState: options.Maximised,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Menu:             appMenu,
		Bind: []any{
			app,
		},
	})

	if err != nil {
		logger.Error("app.fatal", "err", err)
		println("Error:", err.Error())
	}
}

// slogWriter routes Go's default log.Print output through slog at DEBUG level.
type slogWriter struct{ logger *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.logger.Debug("stdlib.log", "msg", string(p))
	return len(p), nil
}
