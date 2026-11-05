package router_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func TestDebouncedNotifier(t *testing.T) {
	t.Run("single trigger emits notification after interval", func(t *testing.T) {
		var received atomic.Pointer[mcp.Message]
		broadcaster := func(msg *mcp.Message) error {
			received.Store(msg)
			return nil
		}

		notifier := router.NewDebouncedNotifier(broadcaster, 20*time.Millisecond)
		defer notifier.Stop()

		notifier.Trigger()

		// Before interval, should not have emitted yet
		time.Sleep(5 * time.Millisecond)
		if received.Load() != nil {
			t.Errorf("notification emitted prematurely")
		}

		// Wait past interval
		time.Sleep(30 * time.Millisecond)
		msg := received.Load()
		if msg == nil {
			t.Fatalf("expected notification to be emitted after interval")
		}

		if msg.Method != mcp.MethodNotificationsToolsListChanged {
			t.Errorf("expected method %q, got %q", mcp.MethodNotificationsToolsListChanged, msg.Method)
		}
		if msg.ID != nil {
			t.Errorf("notification must have nil ID per JSON-RPC 2.0, got %+v", msg.ID)
		}
		if notifier.EmitCount() != 1 {
			t.Errorf("expected EmitCount 1, got %d", notifier.EmitCount())
		}
	})

	t.Run("rapid triggers coalesce into single notification", func(t *testing.T) {
		var emitCount atomic.Int32
		broadcaster := func(msg *mcp.Message) error {
			emitCount.Add(1)
			return nil
		}

		notifier := router.NewDebouncedNotifier(broadcaster, 30*time.Millisecond)
		defer notifier.Stop()

		// Trigger 15 times within 15ms
		for i := 0; i < 15; i++ {
			notifier.Trigger()
			time.Sleep(1 * time.Millisecond)
		}

		// Wait for debounce timer to fire
		time.Sleep(60 * time.Millisecond)

		if emitCount.Load() != 1 {
			t.Fatalf("expected 1 coalesced emission, got %d", emitCount.Load())
		}
		if notifier.EmitCount() != 1 {
			t.Errorf("expected notifier.EmitCount() == 1, got %d", notifier.EmitCount())
		}
	})

	t.Run("Flush emits immediately and clears pending", func(t *testing.T) {
		var emitted atomic.Bool
		broadcaster := func(msg *mcp.Message) error {
			emitted.Store(true)
			return nil
		}

		notifier := router.NewDebouncedNotifier(broadcaster, 500*time.Millisecond)
		defer notifier.Stop()

		// Without Trigger, Flush does nothing
		notifier.Flush()
		if emitted.Load() {
			t.Errorf("Flush should not emit when nothing is pending")
		}

		notifier.Trigger()
		notifier.Flush()

		if !emitted.Load() {
			t.Fatalf("Flush should immediately emit pending notification")
		}
		if notifier.EmitCount() != 1 {
			t.Errorf("expected emitCount 1, got %d", notifier.EmitCount())
		}

		// Second flush should do nothing because pending was cleared
		emitted.Store(false)
		notifier.Flush()
		if emitted.Load() {
			t.Errorf("second Flush should not emit")
		}
	})

	t.Run("Stop aborts pending timer", func(t *testing.T) {
		var emitted atomic.Bool
		broadcaster := func(msg *mcp.Message) error {
			emitted.Store(true)
			return nil
		}

		notifier := router.NewDebouncedNotifier(broadcaster, 20*time.Millisecond)
		notifier.Trigger()
		notifier.Stop()

		time.Sleep(35 * time.Millisecond)
		if emitted.Load() {
			t.Errorf("notification emitted after Stop()")
		}

		// Triggering after Stop should be a no-op
		notifier.Trigger()
		time.Sleep(35 * time.Millisecond)
		if emitted.Load() {
			t.Errorf("notification emitted after Stop() when Trigger called")
		}
	})

	t.Run("dynamic broadcaster update", func(t *testing.T) {
		var firstCalled, secondCalled atomic.Bool

		notifier := router.NewDebouncedNotifier(func(msg *mcp.Message) error {
			firstCalled.Store(true)
			return nil
		}, 10*time.Millisecond)
		defer notifier.Stop()

		notifier.SetBroadcaster(func(msg *mcp.Message) error {
			secondCalled.Store(true)
			return nil
		})

		notifier.Trigger()
		deadline := time.Now().Add(500 * time.Millisecond)
		for !secondCalled.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		if firstCalled.Load() {
			t.Errorf("first broadcaster should not have been called")
		}
		if !secondCalled.Load() {
			t.Errorf("second broadcaster should have been called")
		}
	})

	t.Run("high concurrency stress test", func(t *testing.T) {
		var broadcastTotal atomic.Uint64
		broadcaster := func(msg *mcp.Message) error {
			broadcastTotal.Add(1)
			return nil
		}

		notifier := router.NewDebouncedNotifier(broadcaster, 5*time.Millisecond)
		defer notifier.Stop()

		var wg sync.WaitGroup
		const numGoroutines = 25
		const iters = 50

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					notifier.Trigger()
					if i%10 == 0 {
						notifier.Flush()
					}
					_ = notifier.EmitCount()
				}
			}(g)
		}

		wg.Wait()
		// Allow final timer if pending
		time.Sleep(20 * time.Millisecond)

		if broadcastTotal.Load() == 0 {
			t.Errorf("expected at least 1 broadcast during concurrent triggers")
		}
	})
}
