package sybra

import "net/http"

// WorkerControlHandler exposes app's durable worker transport after database
// initialization. File-backend boards return nil because delivery state must
// survive a leader restart.
func WorkerControlHandler(a *App) http.Handler {
	if a == nil || a.workerControl == nil {
		return nil
	}
	return a.workerControl.Handler()
}
