package router_test

import (
	"fmt"
	"regexp"
	"sync"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func TestGlobToRegex(t *testing.T) {
	tests := []struct {
		pattern  string
		testStr  string
		shouldRe string
		match    bool
	}{
		{
			pattern: "*_delete_*",
			testStr: "supabase_delete_record",
			match:   true,
		},
		{
			pattern: "*_delete_*",
			testStr: "supabase_read_record",
			match:   false,
		},
		{
			pattern: "github/*",
			testStr: "github/create_issue",
			match:   true,
		},
		{
			pattern: "tool?",
			testStr: "tool1",
			match:   true,
		},
		{
			pattern: "tool?",
			testStr: "tool12",
			match:   false,
		},
		{
			pattern: "special.func+name",
			testStr: "special.func+name",
			match:   true,
		},
		{
			pattern: "regex:^custom_[a-z]+$",
			testStr: "custom_tool",
			match:   true,
		},
		{
			pattern: "regex:^custom_[a-z]+$",
			testStr: "custom_123",
			match:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.pattern+"_"+tc.testStr, func(t *testing.T) {
			reStr := router.GlobToRegex(tc.pattern)
			re, err := regexp.Compile(reStr)
			if err != nil {
				t.Fatalf("failed to compile regex %q for pattern %q: %v", reStr, tc.pattern, err)
			}
			matched := re.MatchString(tc.testStr)
			if matched != tc.match {
				t.Errorf("GlobToRegex(%q) -> %q; match(%q) = %v; want %v", tc.pattern, reStr, tc.testStr, matched, tc.match)
			}
		})
	}
}

func TestRegexCache(t *testing.T) {
	t.Run("caching hits and misses", func(t *testing.T) {
		cache := router.NewRegexCache(4)
		if cache.Hits() != 0 || cache.Misses() != 0 {
			t.Fatalf("expected 0 hits and 0 misses initially")
		}

		// First compile -> miss
		re1, err := cache.GetOrCompile("github_*", true)
		if err != nil {
			t.Fatalf("GetOrCompile failed: %v", err)
		}
		if re1 == nil {
			t.Fatalf("expected non-nil regex")
		}
		if cache.Misses() != 1 || cache.Hits() != 0 {
			t.Errorf("expected 1 miss and 0 hits, got %d misses, %d hits", cache.Misses(), cache.Hits())
		}

		// Second lookup -> hit
		re2, err := cache.GetOrCompile("github_*", true)
		if err != nil {
			t.Fatalf("GetOrCompile hit failed: %v", err)
		}
		if re1 != re2 {
			t.Errorf("expected cached regex instance to be identical")
		}
		if cache.Hits() != 1 || cache.Misses() != 1 {
			t.Errorf("expected 1 hit and 1 miss, got %d hits, %d misses", cache.Hits(), cache.Misses())
		}

		// Case sensitivity changes cache key
		_, _ = cache.GetOrCompile("github_*", false)
		if cache.Misses() != 2 {
			t.Errorf("expected 2 misses after case sensitivity flip, got %d", cache.Misses())
		}
	})

	t.Run("warmup pre-compiles patterns", func(t *testing.T) {
		cache := router.NewRegexCache(10)
		patterns := []string{"p1_*", "p2_*", "p3_*"}
		if err := cache.Warmup(patterns, true); err != nil {
			t.Fatalf("Warmup failed: %v", err)
		}

		if cache.Misses() != 3 {
			t.Errorf("expected 3 misses during warmup, got %d", cache.Misses())
		}

		// Now querying them should be all hits
		for _, p := range patterns {
			_, _ = cache.GetOrCompile(p, true)
		}
		if cache.Hits() != 3 {
			t.Errorf("expected 3 hits after warmup, got %d", cache.Hits())
		}
	})

	t.Run("capacity eviction resets cache boundedly", func(t *testing.T) {
		cache := router.NewRegexCache(2)
		_, _ = cache.GetOrCompile("p1", true)
		_, _ = cache.GetOrCompile("p2", true)
		// 3rd entry triggers map clear/eviction
		_, _ = cache.GetOrCompile("p3", true)

		// p1 should now be a miss again
		_, _ = cache.GetOrCompile("p1", true)
		if cache.Misses() != 4 {
			t.Errorf("expected 4 misses total, got %d", cache.Misses())
		}
	})

	t.Run("concurrent access is race free", func(t *testing.T) {
		cache := router.NewRegexCache(64)
		var wg sync.WaitGroup
		const goroutines = 20
		const iters = 50

		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					pat := fmt.Sprintf("pat_%d", (id*iters+i)%10)
					_, err := cache.GetOrCompile(pat, i%2 == 0)
					if err != nil {
						t.Errorf("GetOrCompile concurrent error: %v", err)
					}
				}
			}(g)
		}
		wg.Wait()

		if cache.Hits()+cache.Misses() != goroutines*iters {
			t.Errorf("expected total operations %d, got hits=%d misses=%d", goroutines*iters, cache.Hits(), cache.Misses())
		}
	})
}

func TestToolFilter(t *testing.T) {
	t.Run("initialization with default profiles", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		filter, err := router.NewToolFilter(cfg)
		if err != nil {
			t.Fatalf("NewToolFilter failed: %v", err)
		}

		if filter.CurrentProfile() != router.ProfileFull {
			t.Errorf("expected active profile %q, got %q", router.ProfileFull, filter.CurrentProfile())
		}

		profiles := filter.AvailableProfiles()
		expectedProfiles := []string{router.ProfileDev, router.ProfileFull, router.ProfileMinimal, router.ProfileReadOnly}
		if len(profiles) < len(expectedProfiles) {
			t.Fatalf("expected at least %d profiles, got %d", len(expectedProfiles), len(profiles))
		}
	})

	t.Run("unknown initial active profile returns error", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		cfg.ActiveProfile = "non_existent"
		_, err := router.NewToolFilter(cfg)
		if err == nil {
			t.Fatalf("expected error for unknown active profile")
		}
	})

	t.Run("profile read_only blocks mutating and destructive tools", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		cfg.ActiveProfile = router.ProfileReadOnly
		filter, err := router.NewToolFilter(cfg)
		if err != nil {
			t.Fatalf("NewToolFilter failed: %v", err)
		}

		blocked := []string{
			"delete_file",
			"drop_database",
			"github__create_issue",
			"supabase__update_row",
			"cloudflare_purge_cache",
			"server_kill_process",
			"truncate_logs",
		}
		for _, name := range blocked {
			if filter.IsAllowed(name) {
				t.Errorf("expected tool %q to be blocked under read_only profile", name)
			}
		}

		allowed := []string{
			"get_file",
			"read_user",
			"list_issues",
			"search_code",
			"query_logs",
		}
		for _, name := range allowed {
			if !filter.IsAllowed(name) {
				t.Errorf("expected tool %q to be allowed under read_only profile", name)
			}
		}
	})

	t.Run("global excludes block regardless of profile", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		cfg.ActiveProfile = router.ProfileFull
		cfg.GlobalExclude = []string{"*_internal_*", "secret_*"}
		filter, err := router.NewToolFilter(cfg)
		if err != nil {
			t.Fatalf("NewToolFilter failed: %v", err)
		}

		if filter.IsAllowed("my_internal_tool") {
			t.Errorf("expected my_internal_tool to be blocked by global exclude")
		}
		if filter.IsAllowed("secret_api_key") {
			t.Errorf("expected secret_api_key to be blocked by global exclude")
		}
		if !filter.IsAllowed("public_tool") {
			t.Errorf("expected public_tool to be allowed")
		}
	})

	t.Run("profile minimal includes only discovery tools", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		cfg.ActiveProfile = router.ProfileMinimal
		filter, err := router.NewToolFilter(cfg)
		if err != nil {
			t.Fatalf("NewToolFilter failed: %v", err)
		}

		if !filter.IsAllowed("get_user") || !filter.IsAllowed("list_repos") || !filter.IsAllowed("search_items") {
			t.Errorf("expected discovery tools to be allowed in minimal profile")
		}
		namespacedReadTools := []string{
			"github__get_issue",
			"github__list_issues",
			"fs__read_file",
			"docs__search_pages",
			"web__fetch_url",
		}
		for _, nt := range namespacedReadTools {
			if !filter.IsAllowed(nt) {
				t.Errorf("expected namespaced read tool %q to be allowed in minimal profile", nt)
			}
		}
		if filter.IsAllowed("delete_repo") || filter.IsAllowed("admin_settings") || filter.IsAllowed("deploy_app") || filter.IsAllowed("github__delete_repo") {
			t.Errorf("expected admin/mutating tools to be excluded in minimal profile")
		}
	})

	t.Run("Apply filters tool slice", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		cfg.ActiveProfile = router.ProfileReadOnly
		filter, _ := router.NewToolFilter(cfg)

		tools := []mcp.Tool{
			{Name: "get_user"},
			{Name: "delete_user"},
			{Name: "list_logs"},
			{Name: "drop_table"},
		}

		filtered := filter.Apply(tools)
		if len(filtered) != 2 {
			t.Fatalf("expected 2 filtered tools, got %d", len(filtered))
		}
		if filtered[0].Name != "get_user" || filtered[1].Name != "list_logs" {
			t.Errorf("unexpected filtered tools: %+v", filtered)
		}
	})

	t.Run("dynamic SetProfile and AddProfile", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		filter, _ := router.NewToolFilter(cfg)

		// Switch to read_only
		if err := filter.SetProfile(router.ProfileReadOnly); err != nil {
			t.Fatalf("SetProfile failed: %v", err)
		}
		if filter.CurrentProfile() != router.ProfileReadOnly {
			t.Errorf("expected profile read_only, got %q", filter.CurrentProfile())
		}
		if filter.IsAllowed("delete_user") {
			t.Errorf("delete_user should be blocked under read_only")
		}

		// Switch to unknown profile fails
		if err := filter.SetProfile("does_not_exist"); err == nil {
			t.Fatalf("expected error switching to unknown profile")
		}

		// Add custom profile
		err := filter.AddProfile(router.ProfileConfig{
			Name:        "custom_qa",
			Description: "Custom QA profile",
			Include:     []string{"qa_*", "test_*"},
			Exclude:     []string{"*_danger"},
		})
		if err != nil {
			t.Fatalf("AddProfile failed: %v", err)
		}

		// Empty name fails
		if err := filter.AddProfile(router.ProfileConfig{Name: ""}); err == nil {
			t.Errorf("expected error when adding profile with empty name")
		}

		// Activate custom profile
		if err := filter.SetProfile("custom_qa"); err != nil {
			t.Fatalf("SetProfile custom_qa failed: %v", err)
		}
		if !filter.IsAllowed("qa_verify_system") {
			t.Errorf("expected qa_verify_system to be allowed")
		}
		if filter.IsAllowed("qa_verify_danger") {
			t.Errorf("expected qa_verify_danger to be excluded")
		}
		if filter.IsAllowed("random_tool") {
			t.Errorf("expected random_tool to be excluded (not in include list)")
		}

		stats := filter.Stats()
		if stats.TotalEvaluations == 0 || stats.ProfileSwitches != 2 {
			t.Errorf("unexpected stats: %+v", stats)
		}
	})

	t.Run("high concurrency stress test across profiles", func(t *testing.T) {
		cfg := router.DefaultFilterConfig()
		filter, err := router.NewToolFilter(cfg)
		if err != nil {
			t.Fatalf("NewToolFilter failed: %v", err)
		}

		var wg sync.WaitGroup
		const numGoroutines = 30
		const operationsPerGoroutine = 50

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				profiles := []string{router.ProfileFull, router.ProfileReadOnly, router.ProfileDev, router.ProfileMinimal}
				tools := []mcp.Tool{
					{Name: "get_file"},
					{Name: "delete_record"},
					{Name: "github__create_issue"},
					{Name: "search_docs"},
				}

				for i := 0; i < operationsPerGoroutine; i++ {
					if i%10 == 0 {
						prof := profiles[(gid+i)%len(profiles)]
						_ = filter.SetProfile(prof)
					}
					_ = filter.IsAllowed("delete_record")
					_ = filter.IsAllowed("get_file")
					_ = filter.Apply(tools)
					_ = filter.CurrentProfile()
					_ = filter.AvailableProfiles()
				}
			}(g)
		}

		wg.Wait()

		stats := filter.Stats()
		if stats.TotalEvaluations == 0 {
			t.Errorf("expected evaluations > 0, got %d", stats.TotalEvaluations)
		}
	})
}
