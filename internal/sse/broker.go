// Package sse provides a server-sent events broker that fans out events to
// connected HTTP clients.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	chanBuf           = 256
	heartbeatInterval = 15 * time.Second
)

// Event carries a named event with its JSON-encoded payload.
// Used by ServeAll to multiplex all events over a single SSE stream.
type Event struct {
	Name string
	Data string // JSON-encoded
}

// Broker fans out named events to SSE subscribers.
// The Emit method implements func(string, any) and can be passed directly to
// WithEmit when constructing an App.
type Broker struct {
	mu      sync.RWMutex
	subs    map[string][]chan string
	allSubs []chan Event
}

// New returns an initialised Broker.
func New() *Broker {
	return &Broker{subs: make(map[string][]chan string)}
}

// SubscribeAll returns a channel that receives every emitted event regardless
// of name, plus a cancel function that removes the subscription.
func (b *Broker) SubscribeAll() (ch <-chan Event, cancel func()) {
	inner := make(chan Event, chanBuf)
	ch = inner

	b.mu.Lock()
	b.allSubs = append(b.allSubs, inner)
	b.mu.Unlock()

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, c := range b.allSubs {
			if c == inner {
				b.allSubs = append(b.allSubs[:i], b.allSubs[i+1:]...)
				break
			}
		}
		close(inner)
	}
	return
}

// Emit JSON-encodes data and fans it out to all subscribers of event and to
// all SubscribeAll subscribers.
// Safe for concurrent use. Slow consumers are dropped (non-blocking send).
func (b *Broker) Emit(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	msg := string(payload)

	b.mu.RLock()
	chans := b.subs[event]
	namedSnap := make([]chan string, len(chans))
	copy(namedSnap, chans)
	allSnap := make([]chan Event, len(b.allSubs))
	copy(allSnap, b.allSubs)
	b.mu.RUnlock()

	for _, ch := range namedSnap {
		select {
		case ch <- msg:
		default: // drop slow consumer
		}
	}

	ev := Event{Name: event, Data: msg}
	for _, ch := range allSnap {
		select {
		case ch <- ev:
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

// ServeAll handles GET /events as a multiplexed SSE stream.
// Every emitted event is forwarded using SSE named-event format:
//
//	event: <name>
//	data: <json>
//
// Clients subscribe to specific event types with addEventListener(name, fn).
func (b *Broker) ServeAll(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := b.SubscribeAll()
	defer cancel()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
			flusher.Flush()
		}
	}
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

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := b.Subscribe(event)
	defer cancel()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case msg, open := <-ch:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}
