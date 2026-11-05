package router

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// NotificationBroadcaster broadcasts an MCP notification message to connected clients.
type NotificationBroadcaster func(msg *mcp.Message) error

// DebouncedNotifier coalesces rapid tool mutation events into a single notification.
// Complies with MCP specification by sending notifications/tools/list_changed.
type DebouncedNotifier struct {
	mu          sync.Mutex
	broadcaster NotificationBroadcaster
	interval    time.Duration
	timer       *time.Timer
	pending     bool
	emitCount   atomic.Uint64
	closed      bool
}

// NewDebouncedNotifier creates an initialized DebouncedNotifier.
func NewDebouncedNotifier(broadcaster NotificationBroadcaster, interval time.Duration) *DebouncedNotifier {
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	return &DebouncedNotifier{
		broadcaster: broadcaster,
		interval:    interval,
	}
}

// SetBroadcaster updates or assigns the broadcast sink.
func (n *DebouncedNotifier) SetBroadcaster(b NotificationBroadcaster) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.broadcaster = b
}

// Trigger schedules a debounced emission of notifications/tools/list_changed.
func (n *DebouncedNotifier) Trigger() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.closed {
		return
	}

	n.pending = true
	if n.timer != nil {
		n.timer.Stop()
	}

	n.timer = time.AfterFunc(n.interval, func() {
		n.Flush()
	})
}

// Flush synchronously emits any pending notification immediately.
func (n *DebouncedNotifier) Flush() {
	n.mu.Lock()
	if !n.pending || n.closed || n.broadcaster == nil {
		n.mu.Unlock()
		return
	}

	broadcaster := n.broadcaster
	n.pending = false
	n.mu.Unlock()

	notification, err := mcp.NewNotification(mcp.MethodNotificationsToolsListChanged, nil)
	if err == nil {
		_ = broadcaster(notification)
		n.emitCount.Add(1)
	}
}

// Stop cleanly stops the debouncer and aborts pending timers.
func (n *DebouncedNotifier) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.closed = true
	if n.timer != nil {
		n.timer.Stop()
		n.timer = nil
	}
	n.pending = false
}

// EmitCount returns the total number of notifications emitted.
func (n *DebouncedNotifier) EmitCount() uint64 {
	return n.emitCount.Load()
}
