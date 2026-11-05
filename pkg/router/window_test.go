package router_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

// simpleCatalog implements router.CatalogProvider for test scenarios.
type simpleCatalog struct {
	mu    sync.RWMutex
	tools map[string]mcp.Tool
}

func newSimpleCatalog() *simpleCatalog {
	return &simpleCatalog{
		tools: make(map[string]mcp.Tool),
	}
}

func (c *simpleCatalog) add(t mcp.Tool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools[t.Name] = t
}

func (c *simpleCatalog) GetTool(name string) (mcp.Tool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tools[name]
	return t, ok
}

func (c *simpleCatalog) GetAllTools() []mcp.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := make([]mcp.Tool, 0, len(c.tools))
	for _, t := range c.tools {
		res = append(res, t)
	}
	return res
}

func TestToolWindow(t *testing.T) {
	catalog := newSimpleCatalog()
	for i := 1; i <= 20; i++ {
		catalog.add(mcp.Tool{
			Name:        fmt.Sprintf("tool_%d", i),
			Description: fmt.Sprintf("Description for tool %d", i),
			InputSchema: mcp.InputSchema{Type: "object"},
		})
	}
	// Add meta tools
	catalog.add(mcp.Tool{Name: router.MetaToolSearch, Description: "Search meta"})
	catalog.add(mcp.Tool{Name: router.MetaToolGetSchema, Description: "Schema meta"})

	t.Run("initialization with default config", func(t *testing.T) {
		cfg := router.DefaultWindowConfig()
		w, err := router.NewToolWindow(cfg, catalog, nil)
		if err != nil {
			t.Fatalf("NewToolWindow failed: %v", err)
		}

		stats := w.Stats()
		if stats.MaxActive != 15 {
			t.Errorf("expected MaxActive 15, got %d", stats.MaxActive)
		}
		if stats.PinnedCount != 3 {
			t.Errorf("expected 3 default pinned tools, got %d", stats.PinnedCount)
		}
		if len(w.ActiveTools()) != 0 {
			t.Errorf("expected 0 active tools initially, got %d", len(w.ActiveTools()))
		}
	})

	t.Run("promote tool into active window", func(t *testing.T) {
		var triggered atomic.Bool
		notifier := router.NewDebouncedNotifier(func(msg *mcp.Message) error {
			triggered.Store(true)
			return nil
		}, 10*time.Millisecond)
		defer notifier.Stop()

		cfg := router.WindowConfig{MaxActive: 3}
		w, _ := router.NewToolWindow(cfg, catalog, notifier)

		promoted, evicted, err := w.Promote("tool_1")
		if err != nil || !promoted || evicted != "" {
			t.Fatalf("expected promote success without eviction: promoted=%v, evicted=%s, err=%v", promoted, evicted, err)
		}

		if !w.IsActive("tool_1") {
			t.Errorf("tool_1 should be active")
		}

		// Promoting already active tool returns false
		promotedAgain, _, err := w.Promote("tool_1")
		if err != nil || promotedAgain {
			t.Errorf("expected promoted=false for already active tool")
		}

		// Promoting tool not in catalog returns error
		_, _, err = w.Promote("non_existent_tool")
		if err == nil {
			t.Errorf("expected error for promoting tool not in catalog")
		}

		deadline := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(deadline) && !triggered.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		if !triggered.Load() {
			t.Errorf("expected notifier to trigger on promotion")
		}
	})

	t.Run("LRU eviction when unpinned capacity is reached", func(t *testing.T) {
		cfg := router.WindowConfig{MaxActive: 3}
		w, _ := router.NewToolWindow(cfg, catalog, nil)

		// Promote 3 unpinned tools: tool_1, tool_2, tool_3
		_, _, _ = w.Promote("tool_1")
		_, _, _ = w.Promote("tool_2")
		_, _, _ = w.Promote("tool_3")

		// Active should have [tool_3, tool_2, tool_1] (tool_1 is LRU)
		active := w.ActiveTools()
		if len(active) != 3 {
			t.Fatalf("expected 3 active tools, got %d", len(active))
		}
		if active[0].Name != "tool_3" || active[2].Name != "tool_1" {
			t.Errorf("unexpected LRU ordering: %+v", active)
		}

		// Promoting tool_4 should evict tool_1
		promoted, evicted, err := w.Promote("tool_4")
		if err != nil || !promoted {
			t.Fatalf("promote tool_4 failed: %v", err)
		}
		if evicted != "tool_1" {
			t.Errorf("expected evicted tool_1, got %q", evicted)
		}
		if w.IsActive("tool_1") {
			t.Errorf("tool_1 should no longer be active")
		}
		if !w.IsActive("tool_4") {
			t.Errorf("tool_4 should now be active")
		}

		// Active should now be [tool_4, tool_3, tool_2]
		active = w.ActiveTools()
		if active[0].Name != "tool_4" || active[2].Name != "tool_2" {
			t.Errorf("unexpected ordering after eviction: %+v", active)
		}

		// Touching tool_2 moves it to MRU (head)
		w.RecordUsage("tool_2")
		active = w.ActiveTools()
		if active[0].Name != "tool_2" {
			t.Errorf("tool_2 should be at head after RecordUsage, got: %s", active[0].Name)
		}

		// Now tool_3 is LRU. Promoting tool_5 should evict tool_3!
		_, evicted, _ = w.Promote("tool_5")
		if evicted != "tool_3" {
			t.Errorf("expected evicted tool_3, got %q", evicted)
		}
	})

	t.Run("pinned tools are immune to LRU eviction", func(t *testing.T) {
		cfg := router.WindowConfig{
			MaxActive:   2,
			PinnedTools: []string{router.MetaToolSearch},
		}
		w, _ := router.NewToolWindow(cfg, catalog, nil)

		// Promote pinned tool and 2 unpinned tools
		_, _, _ = w.Promote(router.MetaToolSearch) // pinned
		_, _, _ = w.Promote("tool_1")              // unpinned 1
		_, _, _ = w.Promote("tool_2")              // unpinned 2

		// Total active is 3 (1 pinned + 2 unpinned)
		if len(w.ActiveTools()) != 3 {
			t.Fatalf("expected 3 active tools (1 pinned + 2 unpinned), got %d", len(w.ActiveTools()))
		}

		// Promote unpinned tool_3. Since unpinned count is 2 (== MaxActive 2),
		// it must evict tool_1 (the LRU unpinned tool), NOT MetaToolSearch!
		_, evicted, _ := w.Promote("tool_3")
		if evicted != "tool_1" {
			t.Errorf("expected evicted tool_1, got %q", evicted)
		}
		if !w.IsActive(router.MetaToolSearch) {
			t.Errorf("pinned tool should never be evicted")
		}

		// Dynamically Pin and Unpin
		w.Pin("tool_2")
		w.Unpin(router.MetaToolSearch)
		// Now tool_2 is pinned, MetaToolSearch is unpinned
		// Promote tool_4: MetaToolSearch is LRU unpinned, so it can now be evicted
		_, evicted, _ = w.Promote("tool_4")
		if evicted != router.MetaToolSearch {
			t.Errorf("expected newly unpinned tool to be evictable, got evicted %q", evicted)
		}
	})

	t.Run("Demote removes tool from active window", func(t *testing.T) {
		cfg := router.WindowConfig{MaxActive: 5}
		w, _ := router.NewToolWindow(cfg, catalog, nil)

		_, _, _ = w.Promote("tool_1")
		_, _, _ = w.Promote("tool_2")

		if !w.Demote("tool_1") {
			t.Errorf("expected Demote(tool_1) to return true")
		}
		if w.IsActive("tool_1") {
			t.Errorf("tool_1 should not be active after demotion")
		}

		// Demoting already demoted tool returns false
		if w.Demote("tool_1") {
			t.Errorf("expected Demote(tool_1) to return false on duplicate call")
		}

		stats := w.Stats()
		if stats.Demotions != 1 {
			t.Errorf("expected 1 demotion in stats, got %d", stats.Demotions)
		}
	})

	t.Run("AutoPromoteOnCall promotes tool when RecordUsage called on dormant tool", func(t *testing.T) {
		cfg := router.WindowConfig{
			MaxActive:         5,
			AutoPromoteOnCall: true,
		}
		w, _ := router.NewToolWindow(cfg, catalog, nil)

		if w.IsActive("tool_10") {
			t.Errorf("tool_10 should be dormant initially")
		}

		w.RecordUsage("tool_10")

		if !w.IsActive("tool_10") {
			t.Errorf("tool_10 should have been auto-promoted on usage touch")
		}

		stats := w.Stats()
		if stats.Promotions != 1 {
			t.Errorf("expected 1 promotion in stats, got %d", stats.Promotions)
		}
	})

	t.Run("high concurrency stress test across promotions and evictions", func(t *testing.T) {
		cfg := router.WindowConfig{
			MaxActive:         8,
			AutoPromoteOnCall: true,
			PinnedTools:       []string{router.MetaToolSearch},
		}
		w, _ := router.NewToolWindow(cfg, catalog, nil)

		var wg sync.WaitGroup
		const numGoroutines = 25
		const iters = 40

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					tName := fmt.Sprintf("tool_%d", (gid+i)%20+1)
					if i%3 == 0 {
						_, _, _ = w.Promote(tName)
					} else if i%3 == 1 {
						w.RecordUsage(tName)
					} else {
						_ = w.Demote(tName)
					}
					_ = w.ActiveTools()
					_ = w.IsActive(tName)
					_ = w.Stats()
				}
			}(g)
		}

		wg.Wait()

		stats := w.Stats()
		if stats.Promotions == 0 || stats.UsageTouches == 0 {
			t.Errorf("unexpected empty stats after concurrent run: %+v", stats)
		}
	})
}
