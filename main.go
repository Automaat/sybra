package main

import (
	"embed"

	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/logging"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		println("Error loading config:", err.Error())
		return
	}

	logger, cleanup, err := logging.New(cfg.Logging)
	if err != nil {
		println("Error initializing logger:", err.Error())
		return
	}
	defer cleanup()

	app := NewApp(logger, cfg.Logging.Dir, cfg.TasksDir)

	err = wails.Run(&options.App{
		Title:  "Synapse",
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []any{
			app,
		},
	})

	if err != nil {
		logger.Error("app.fatal", "err", err)
		println("Error:", err.Error())
	}
}
