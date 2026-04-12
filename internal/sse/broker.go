// Package sse provides a server-sent events broker that fans out events to
// connected HTTP clients.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

const chanBuf = 256

// Broker fans out named events to SSE subscribers.
// The Emit method implements func(string, any) and can be passed directly to
// WithEmit when constructing an App.
type Broker struct {
	mu   sync.RWMutex
	subs map[string][]chan string
}

// New returns an initialised Broker.
func New() *Broker {
	return &Broker{subs: make(map[string][]chan string)}
}

// Emit JSON-encodes data and fans it out to all subscribers of event.
// Safe for concurrent use. Slow consumers are dropped (non-blocking send).
func (b *Broker) Emit(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := string(payload)

	b.mu.RLock()
	chans := b.subs[event]
	// Copy slice so we can iterate outside the lock.
	snapshot := make([]chan string, len(chans))
	copy(snapshot, chans)
	b.mu.RUnlock()

	for _, ch := range snapshot {
		select {
		case ch <- msg:
		default: // drop slow consumer
		}
	}
}

// Subscribe returns a receive channel for the named event and a cancel function
// that removes the subscription.
func (b *Broker) Subscribe(event string) (ch <-chan string, cancel func()) {
	inner := make(chan string, chanBuf)
	ch = inner

	b.mu.Lock()
	b.subs[event] = append(b.subs[event], inner)
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		chans := b.subs[event]
		for i, c := range chans {
			if c == inner {
				b.subs[event] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		close(inner)
	}
	return
}

// ServeHTTP handles GET /api/events/{eventName} as an SSE stream.
// The event name is taken from the last path segment.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract event name from the last path segment.
	event := r.PathValue("eventName")
	if event == "" {
		http.Error(w, "missing eventName", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	ch, cancel := b.Subscribe(event)
	defer cancel()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, open := <-ch:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}
