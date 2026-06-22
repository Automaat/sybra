package sybra

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/notification"
)

// NotifyUIStall surfaces a stalled desktop UI via an OS-native notification
// that bypasses the (frozen) webview event path. Called by the desktop emitter
// watchdog when the frontend has received no events for a sustained period —
// the in-app notification stream cannot deliver this, so it must go direct.
func (a *App) NotifyUIStall(d time.Duration) {
	if a.notifier == nil {
		return
	}
	a.notifier.Alert(notification.LevelError, "Sybra UI stalled",
		fmt.Sprintf("No UI updates for %s — the window is likely frozen. Restart Sybra to recover; the backend keeps running.",
			d.Round(time.Second)))
}

// NotifyUIRecovered announces that the desktop UI event path resumed after a
// stall, again via the OS-native path so the message is consistent with the
// stall alert.
func (a *App) NotifyUIRecovered(d time.Duration) {
	if a.notifier == nil {
		return
	}
	a.notifier.Alert(notification.LevelSuccess, "Sybra UI recovered",
		fmt.Sprintf("UI updates resumed after %s.", d.Round(time.Second)))
}
