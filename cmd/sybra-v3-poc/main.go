// Command sybra-v3-poc is the Phase 1 spike for the Wails v2 → v3 migration.
//
// Not shipped. Not on main. See docs/migrations/wails-v3.md for the plan.
//
// Goal of the spike: prove that application.New + WindowManager.NewWithOptions
// + a single v3 service compile and run end-to-end against the existing
// embedded asset pattern. The frontend page calls InfoService.GetVersion
// via the v3 runtime (Call.ByName) — no generated bindings needed.
//
// Gated darwin-only because Wails v3 alpha needs gtk3/webkit2gtk-4.1 to
// compile on Linux; CI runners lack those headers. See main_other.go for
// the no-op stub that keeps `go build ./...` green on Linux.

//go:build darwin

package main

import (
	"embed"
	"log"
	"log/slog"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/Automaat/sybra/internal/sybra/v3svc"
)

//go:embed all:dist
var assets embed.FS

func main() {
	app := application.New(application.Options{
		Name:        "Sybra (v3 POC)",
		Description: "Wails v3 migration spike",
		LogLevel:    slog.LevelDebug,
		Services: []application.Service{
			application.NewService(v3svc.NewInfoService()),
		},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Sybra v3 POC",
		Width:  640,
		Height: 360,
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
