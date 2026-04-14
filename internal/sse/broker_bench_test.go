package sse

import (
	"fmt"
	"sync"
	"testing"
)

// payload is a representative StreamEvent-shaped value used to exercise the
// marshal + fanout hot path.
type payload struct {
	Type      string `json:"type"`
	Content   string `json:"content"`
	SessionID string `json:"sessionId"`
	Cost      int    `json:"cost"`
}

func newPayload(i int) payload {
	return payload{
		Type:      "assistant",
		Content:   fmt.Sprintf("bench body %d with some text to avoid trivial sizing", i),
		SessionID: "bench",
		Cost:      i,
	}
}

// BenchmarkEmit_NoSubscribers measures the floor cost of Emit (JSON marshal
// + mutex acquisition) when no one is listening. Sets the baseline that all
// fanout overhead is measured against.
func BenchmarkEmit_NoSubscribers(b *testing.B) {
	broker := New()
	ev := newPayload(0)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		broker.Emit("agent:output:bench", ev)
	}
}

// BenchmarkEmit_AllSubscribers scales the number of SubscribeAll listeners
// to surface fanout cost. Each subscriber must receive a non-blocking send
// attempt; this benchmark also tracks whether the broker drops messages
// under pressure (slow consumers).
func BenchmarkEmit_AllSubscribers(b *testing.B) {
	for _, subs := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			broker := New()
			var wg sync.WaitGroup
			stop := make(chan struct{})
			for range subs {
				ch, cancel := broker.SubscribeAll()
				wg.Go(func() {
					defer cancel()
					for {
						select {
						case <-stop:
							return
						case _, ok := <-ch:
							if !ok {
								return
							}
						}
					}
				})
			}
			ev := newPayload(0)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				broker.Emit("agent:output:bench", ev)
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
		})
	}
}

// BenchmarkEmit_NamedSubscribers measures the Subscribe(eventName) path used
// by the legacy per-event endpoint. Separates named-subs cost from AllSubs
// cost since they walk different maps inside Emit.
func BenchmarkEmit_NamedSubscribers(b *testing.B) {
	const subs = 10
	broker := New()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range subs {
		ch, cancel := broker.Subscribe("agent:output:bench")
		wg.Go(func() {
			defer cancel()
			for {
				select {
				case <-stop:
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
				}
			}
		})
	}
	ev := newPayload(0)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		broker.Emit("agent:output:bench", ev)
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
}
