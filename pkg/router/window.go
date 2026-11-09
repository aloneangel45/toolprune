package router

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

var (
	// ErrToolNotInCatalog is returned when attempting to promote a tool that does not exist.
	ErrToolNotInCatalog = errors.New("tool not registered in catalog")
	// ErrWindowZeroCap is returned when window capacity is <= 0.
	ErrWindowZeroCap = errors.New("active window max capacity must be positive")
)

// WindowConfig defines sliding window parameters.
type WindowConfig struct {
	Enabled           bool     `json:"enabled" yaml:"enabled"`
	MaxActive         int      `json:"maxActive" yaml:"max_active"`                   // Default: 15
	AutoPromoteOnCall bool     `json:"autoPromoteOnCall" yaml:"auto_promote_on_call"` // Default: true
	PinnedTools       []string `json:"pinnedTools" yaml:"pinned_tools"`               // Immune to LRU eviction
}

// DefaultWindowConfig returns production defaults for active tool windowing.
func DefaultWindowConfig() WindowConfig {
	return WindowConfig{
		Enabled:           true,
		MaxActive:         15,
		AutoPromoteOnCall: true,
		PinnedTools: []string{
			MetaToolSearch,
			MetaToolGetSchema,
			MetaToolActivate,
		},
	}
}

// CatalogProvider provides tool lookup for window promotion.
type CatalogProvider interface {
	GetTool(name string) (mcp.Tool, bool)
	GetAllTools() []mcp.Tool
}

// WindowStats tracks active window metrics.
type WindowStats struct {
	MaxActive    int
	ActiveCount  int
	PinnedCount  int
	Promotions   uint64
	Demotions    uint64
	Evictions    uint64
	UsageTouches uint64
}

type windowNode struct {
	name       string
	tool       mcp.Tool
	isPinned   bool
	lastUsed   time.Time
	usageCount uint64
	prev       *windowNode
	next       *windowNode
}

// ToolWindow manages the sliding window of active tools using an O(1) LRU list.
type ToolWindow struct {
	mu           sync.RWMutex
	cfg          WindowConfig
	catalog      CatalogProvider
	notifier     *DebouncedNotifier
	activeMap    map[string]*windowNode
	pinnedMap    map[string]struct{}
	head         *windowNode // Most Recently Used (MRU)
	tail         *windowNode // Least Recently Used (LRU)
	promotions   atomic.Uint64
	demotions    atomic.Uint64
	evictions    atomic.Uint64
	usageTouches atomic.Uint64
}

// NewToolWindow creates an initialized ToolWindow.
func NewToolWindow(cfg WindowConfig, catalog CatalogProvider, notifier *DebouncedNotifier) (*ToolWindow, error) {
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 15
	}

	w := &ToolWindow{
		cfg:       cfg,
		catalog:   catalog,
		notifier:  notifier,
		activeMap: make(map[string]*windowNode),
		pinnedMap: make(map[string]struct{}),
	}

	for _, name := range cfg.PinnedTools {
		w.pinnedMap[name] = struct{}{}
	}

	return w, nil
}

// SetCatalog assigns or updates the catalog provider.
func (w *ToolWindow) SetCatalog(catalog CatalogProvider) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.catalog = catalog
}

// SetNotifier assigns or updates the debounced notifier.
func (w *ToolWindow) SetNotifier(notifier *DebouncedNotifier) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notifier = notifier
}

// ActiveTools returns a slice of currently active tool definitions.
func (w *ToolWindow) ActiveTools() []mcp.Tool {
	w.mu.RLock()
	defer w.mu.RUnlock()

	tools := make([]mcp.Tool, 0, len(w.activeMap))
	curr := w.head
	for curr != nil {
		tools = append(tools, curr.tool)
		curr = curr.next
	}
	return tools
}

// IsActive checks whether a tool is currently present in the active window.
func (w *ToolWindow) IsActive(toolName string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.activeMap[toolName]
	return ok
}

// RecordUsage touches a tool to refresh its LRU position and usage stats.
func (w *ToolWindow) RecordUsage(toolName string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.usageTouches.Add(1)

	node, ok := w.activeMap[toolName]
	if ok {
		node.usageCount++
		node.lastUsed = time.Now()
		w.moveToHead(node)
		return
	}

	// If not active, auto-promote if enabled
	if w.cfg.AutoPromoteOnCall && w.catalog != nil {
		promoted, _, _ := w.promoteUnderLock(toolName)
		if promoted && w.notifier != nil {
			w.notifier.Trigger()
		}
	}
}

// Promote elevates a tool to the active window. Returns true if promoted, false if already active.
func (w *ToolWindow) Promote(toolName string) (promoted bool, evicted string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	promoted, evicted, err = w.promoteUnderLock(toolName)
	if promoted && w.notifier != nil {
		w.notifier.Trigger()
	}
	return promoted, evicted, err
}

func (w *ToolWindow) promoteUnderLock(toolName string) (bool, string, error) {
	if node, ok := w.activeMap[toolName]; ok {
		node.lastUsed = time.Now()
		node.usageCount++
		w.moveToHead(node)
		return false, "", nil
	}

	if w.catalog == nil {
		return false, "", fmt.Errorf("%w: catalog is nil", ErrToolNotInCatalog)
	}

	tool, exists := w.catalog.GetTool(toolName)
	if !exists {
		return false, "", fmt.Errorf("%w: %q", ErrToolNotInCatalog, toolName)
	}

	_, isPinned := w.pinnedMap[toolName]

	var evictedName string
	// Check if active unpinned tools meet or exceed MaxActive (pinned tools must not evict unpinned tools)
	if !isPinned && w.unpinnedCount() >= w.cfg.MaxActive {
		evictedNode := w.findEvictableTail()
		if evictedNode != nil {
			evictedName = evictedNode.name
			w.removeNode(evictedNode)
			delete(w.activeMap, evictedName)
			w.evictions.Add(1)
		}
	}
	newNode := &windowNode{
		name:       toolName,
		tool:       tool,
		isPinned:   isPinned,
		lastUsed:   time.Now(),
		usageCount: 1,
	}

	w.activeMap[toolName] = newNode
	w.addToHead(newNode)
	w.promotions.Add(1)

	return true, evictedName, nil
}

// Demote removes a tool from the active window back to dormant status.
func (w *ToolWindow) Demote(toolName string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	node, ok := w.activeMap[toolName]
	if !ok {
		return false
	}

	w.removeNode(node)
	delete(w.activeMap, toolName)
	w.demotions.Add(1)

	if w.notifier != nil {
		w.notifier.Trigger()
	}
	return true
}

// Pin prevents a tool from being evicted by LRU overflow.
func (w *ToolWindow) Pin(toolName string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pinnedMap[toolName] = struct{}{}
	if node, ok := w.activeMap[toolName]; ok {
		node.isPinned = true
	}
}

// Unpin removes pinned status from a tool.
func (w *ToolWindow) Unpin(toolName string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.pinnedMap, toolName)
	if node, ok := w.activeMap[toolName]; ok {
		node.isPinned = false
	}
}

// Helper linked-list operations (under lock)
func (w *ToolWindow) addToHead(node *windowNode) {
	node.next = w.head
	node.prev = nil
	if w.head != nil {
		w.head.prev = node
	}
	w.head = node
	if w.tail == nil {
		w.tail = node
	}
}

func (w *ToolWindow) removeNode(node *windowNode) {
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		w.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else {
		w.tail = node.prev
	}
	node.prev = nil
	node.next = nil
}

func (w *ToolWindow) moveToHead(node *windowNode) {
	if w.head == node {
		return
	}
	w.removeNode(node)
	w.addToHead(node)
}

func (w *ToolWindow) findEvictableTail() *windowNode {
	curr := w.tail
	for curr != nil {
		if !curr.isPinned {
			return curr
		}
		curr = curr.prev
	}
	return nil
}

func (w *ToolWindow) unpinnedCount() int {
	count := 0
	for _, node := range w.activeMap {
		if !node.isPinned {
			count++
		}
	}
	return count
}

// Stats returns a metrics snapshot.
func (w *ToolWindow) Stats() WindowStats {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return WindowStats{
		MaxActive:    w.cfg.MaxActive,
		ActiveCount:  len(w.activeMap),
		PinnedCount:  len(w.pinnedMap),
		Promotions:   w.promotions.Load(),
		Demotions:    w.demotions.Load(),
		Evictions:    w.evictions.Load(),
		UsageTouches: w.usageTouches.Load(),
	}
}
