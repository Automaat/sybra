package sybra

import "github.com/Automaat/sybra/internal/spotlight"

func (a *App) registerSpotlight(callback func()) {
	if err := spotlight.Register(callback); err != nil {
		a.logger.Error("spotlight.register", "err", err)
		return
	}
	a.logger.Info("spotlight.registered", "hotkey", "ctrl+space")
}
