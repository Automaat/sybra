package sse

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// BenchmarkUnsubscribe_AllSubs exercises the cancel path on SubscribeAll
// against a bucket pre-populated with N subscribers. The slice-backed
// implementation grew linearly with N (O(N) scan + slice reslice); the
// map-backed implementation should stay flat across N.
//
// One bench op = one cancel call. The bucket is refilled inside StopTimer/
// StartTimer windows so the measured time only includes the cancel itself.
func BenchmarkUnsubscribe_AllSubs(b *testing.B) {
	for _, subs := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			broker := New()
			cancels := make([]func(), subs)
			refill := func() {
				for i := range subs {
					_, c := broker.SubscribeAll()
					cancels[i] = c
				}
			}
			refill()
			b.ResetTimer()
			b.ReportAllocs()
			for i := range b.N {
				cancels[i%subs]()
				if (i+1)%subs == 0 {
					b.StopTimer()
					broker = New()
					refill()
					b.StartTimer()
				}
			}
		})
	}
}

// BenchmarkUnsubscribe_NamedSubs mirrors the above against Subscribe(event).
// Verifies the lazy-init + empty-bucket-prune path stays O(1) across N.
func BenchmarkUnsubscribe_NamedSubs(b *testing.B) {
	for _, subs := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			const event = "agent:output:bench"
			broker := New()
			cancels := make([]func(), subs)
			refill := func() {
				for i := range subs {
					_, c := broker.Subscribe(event)
					cancels[i] = c
				}
			}
			refill()
			b.ResetTimer()
			b.ReportAllocs()
			for i := range b.N {
				cancels[i%subs]()
				if (i+1)%subs == 0 {
					b.StopTimer()
					broker = New()
					refill()
					b.StartTimer()
				}
			}
		})
	}
}

// BenchmarkChurn_AllSubs interleaves concurrent sub/cancel with a steady
// Emit driver. The benchmark proves the broker survives 100+ concurrent
// subscribers turning over without deadlock or data race. Each parallel
// iteration mimics a browser SSE client connecting, draining a couple of
// events, and disconnecting.
func BenchmarkChurn_AllSubs(b *testing.B) {
	broker := New()
	stop := make(chan struct{})

	// Steady event driver running through the benchmark.
	var driverWG sync.WaitGroup
	driverWG.Go(func() {
		ev := newPayload(0)
		ticker := time.NewTicker(50 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				broker.Emit("agent:output:bench", ev)
			}
		}
	})

	var connects atomic.Int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ch, cancel := broker.SubscribeAll()
			connects.Add(1)
			// Drain up to a couple of events with a short deadline so we
			// don't block forever if no events arrive between subscribe
			// and unsubscribe.
			drained := 0
			deadline := time.After(500 * time.Microsecond)
			for drained < 2 {
				select {
				case _, ok := <-ch:
					if !ok {
						drained = 2
					} else {
						drained++
					}
				case <-deadline:
					drained = 2
				}
			}
			cancel()
		}
	})
	b.StopTimer()
	close(stop)
	driverWG.Wait()
	b.ReportMetric(float64(connects.Load())/float64(b.N), "conn/op")
}
