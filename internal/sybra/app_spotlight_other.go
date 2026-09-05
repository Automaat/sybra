//go:build !darwin

package sybra

import "github.com/Automaat/sybra/internal/spotlight"

func (a *App) registerSpotlight(callback func()) {
	// The non-Darwin implementation always returns the platform support error.
	// Logging it directly avoids a comparison that staticcheck can prove true.
	a.logger.Error("spotlight.register", "err", spotlight.Register(callback))
}
