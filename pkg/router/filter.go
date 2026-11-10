package router

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// Standard built-in profile names.
const (
	ProfileFull     = "full"
	ProfileReadOnly = "read_only"
	ProfileDev      = "dev"
	ProfileMinimal  = "minimal"
)

var (
	// ErrProfileNotFound is returned when an unknown profile name is requested.
	ErrProfileNotFound = errors.New("profile not found")
	// ErrEmptyProfileName is returned when attempting to register a profile with an empty name.
	ErrEmptyProfileName = errors.New("profile name cannot be empty")
)

// ProfileConfig defines tool inclusion and exclusion patterns for a named profile.
type ProfileConfig struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Include     []string `json:"include" yaml:"include"`
	Exclude     []string `json:"exclude" yaml:"exclude"`
}

// FilterConfig holds global filtering rules and profile definitions.
type FilterConfig struct {
	ActiveProfile   string                   `json:"activeProfile" yaml:"active_profile"`
	CaseInsensitive bool                     `json:"caseInsensitive" yaml:"case_insensitive"`
	GlobalExclude   []string                 `json:"globalExclude" yaml:"global_exclude"`
	Profiles        map[string]ProfileConfig `json:"profiles" yaml:"profiles"`
}

// DefaultFilterConfig provides standard production defaults for all built-in profiles.
func DefaultFilterConfig() FilterConfig {
	return FilterConfig{
		ActiveProfile:   ProfileFull,
		CaseInsensitive: true,
		GlobalExclude:   []string{"*_internal_*"},
		Profiles: map[string]ProfileConfig{
			ProfileFull: {
				Name:        ProfileFull,
				Description: "Pass-through all registered tools without filtering",
				Include:     []string{"*"},
				Exclude:     nil,
			},
			ProfileReadOnly: {
				Name:        ProfileReadOnly,
				Description: "Blocks all destructive and mutating tools",
				Include:     []string{"*"},
				Exclude: []string{
					"*_delete_*", "*_drop_*", "*_remove_*", "*_destroy_*", "*_purge_*", "*_truncate_*", "*_kill_*",
					"*_create_*", "*_update_*", "*_insert_*", "*_edit_*", "*_deploy_*", "*_pause_*", "*_restore_*",
					"*_reset_*", "*_rebase_*",
					"*__delete_*", "*__drop_*", "*__remove_*", "*__destroy_*", "*__purge_*", "*__truncate_*", "*__kill_*",
					"*__create_*", "*__update_*", "*__insert_*", "*__edit_*", "*__deploy_*", "*__pause_*", "*__restore_*",
					"*__reset_*", "*__rebase_*",
					"*_delete", "*_drop", "*_remove", "*_destroy", "*_purge", "*_truncate", "*_kill",
					"*_create", "*_update", "*_insert", "*_edit", "*_deploy", "*_pause", "*_restore",
					"*_reset", "*_rebase",
					"*__delete", "*__drop", "*__remove", "*__destroy", "*__purge", "*__truncate", "*__kill",
					"*__create", "*__update", "*__insert", "*__edit", "*__deploy", "*__pause", "*__restore",
					"*__reset", "*__rebase",
					"delete_*", "drop_*", "remove_*", "destroy_*", "purge_*", "truncate_*", "kill_*",
					"create_*", "update_*", "insert_*", "edit_*", "deploy_*", "pause_*", "restore_*",
					"reset_*", "rebase_*",
					"delete", "drop", "remove", "destroy", "purge", "truncate", "kill",
					"create", "update", "insert", "edit", "deploy", "pause", "restore",
					"reset", "rebase",
				},
			},
			ProfileDev: {
				Name:        ProfileDev,
				Description: "Standard software development tools (git, devtools, filesystem, context)",
				Include: []string{
					"github/*", "filesystem/*", "chrome-devtools/*", "context7/*",
					"github__*", "filesystem__*", "devtools__*", "chrome-devtools__*", "context7__*",
					"git_*", "file_*", "code_*", "search_*", "read_*", "list_*", "get_*", "fetch_*",
					"*__git_*", "*__file_*", "*__code_*", "*__search_*", "*__read_*", "*__list_*", "*__get_*", "*__fetch_*",
				},
				Exclude: []string{
					"cloudflare/delete_*", "supabase/delete_*", "*_drop_*", "*_purge_*",
				},
			},
			ProfileMinimal: {
				Name:        ProfileMinimal,
				Description: "Query and discovery tools only",
				Include: []string{
					"get_*", "read_*", "list_*", "search_*", "query_*", "fetch_*",
					"*__get_*", "*__list_*", "*__read_*", "*__search_*", "*__fetch_*", "*__query_*",
					"*get*", "*read*", "*list*", "*search*", "*query*", "*fetch*",
				},
				Exclude: []string{
					"*admin*", "*manage*", "*config*", "*deploy*", "*secret*", "*delete*", "*drop*",
				},
			},
		},
	}
}

// CompiledProfile caches pre-compiled regular expressions for a profile.
type CompiledProfile struct {
	Config   ProfileConfig
	Includes []*regexp.Regexp
	Excludes []*regexp.Regexp
}

// FilterStats holds operational metrics for the filtering engine.
type FilterStats struct {
	TotalEvaluations uint64
	AllowedCount     uint64
	FilteredCount    uint64
	ProfileSwitches  uint64
}

// ToolFilter evaluates tools against active profiles and pattern rules.
type ToolFilter struct {
	mu              sync.RWMutex
	activeProfile   string
	caseInsensitive bool
	globalExcludes  []*regexp.Regexp
	profiles        map[string]*CompiledProfile
	regexCache      *RegexCache
	totalEvals      atomic.Uint64
	allowedCount    atomic.Uint64
	filteredCount   atomic.Uint64
	profileSwitches atomic.Uint64
}

// NewToolFilter compiles patterns and initializes the ToolFilter.
func NewToolFilter(cfg FilterConfig) (*ToolFilter, error) {
	if cfg.ActiveProfile == "" {
		cfg.ActiveProfile = ProfileFull
	}

	cache := NewRegexCache(512)
	filter := &ToolFilter{
		activeProfile:   cfg.ActiveProfile,
		caseInsensitive: cfg.CaseInsensitive,
		profiles:        make(map[string]*CompiledProfile),
		regexCache:      cache,
	}

	// Compile global excludes
	for _, pat := range cfg.GlobalExclude {
		re, err := cache.GetOrCompile(pat, cfg.CaseInsensitive)
		if err != nil {
			return nil, fmt.Errorf("invalid global_exclude pattern %q: %w", pat, err)
		}
		filter.globalExcludes = append(filter.globalExcludes, re)
	}

	// Merge default profiles if missing
	defaultCfg := DefaultFilterConfig()
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]ProfileConfig)
	}
	for k, defProf := range defaultCfg.Profiles {
		if _, ok := cfg.Profiles[k]; !ok {
			cfg.Profiles[k] = defProf
		}
	}

	// Compile each profile
	for name, prof := range cfg.Profiles {
		compiled, err := filter.compileProfile(prof)
		if err != nil {
			return nil, fmt.Errorf("failed to compile profile %q: %w", name, err)
		}
		filter.profiles[name] = compiled
	}

	// Verify active profile exists
	if _, ok := filter.profiles[filter.activeProfile]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrProfileNotFound, filter.activeProfile)
	}

	return filter, nil
}

func (f *ToolFilter) compileProfile(prof ProfileConfig) (*CompiledProfile, error) {
	compiled := &CompiledProfile{
		Config: prof,
	}
	for _, inc := range prof.Include {
		re, err := f.regexCache.GetOrCompile(inc, f.caseInsensitive)
		if err != nil {
			return nil, fmt.Errorf("invalid include pattern %q: %w", inc, err)
		}
		compiled.Includes = append(compiled.Includes, re)
	}
	for _, exc := range prof.Exclude {
		re, err := f.regexCache.GetOrCompile(exc, f.caseInsensitive)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", exc, err)
		}
		compiled.Excludes = append(compiled.Excludes, re)
	}
	return compiled, nil
}

// Apply filters a slice of tools according to the active profile and global exclusion rules.
func (f *ToolFilter) Apply(tools []mcp.Tool) []mcp.Tool {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make([]mcp.Tool, 0, len(tools))
	for _, tool := range tools {
		if f.isAllowedUnderLock(tool.Name) {
			result = append(result, tool)
		}
	}
	return result
}

// IsAllowed checks whether a single tool name is permitted under the active profile.
func (f *ToolFilter) IsAllowed(toolName string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.isAllowedUnderLock(toolName)
}

func (f *ToolFilter) isAllowedUnderLock(toolName string) bool {
	f.totalEvals.Add(1)

	// 1. Check Global Excludes
	for _, re := range f.globalExcludes {
		if re.MatchString(toolName) {
			f.filteredCount.Add(1)
			return false
		}
	}

	// 2. Lookup Active Profile
	prof, ok := f.profiles[f.activeProfile]
	if !ok {
		// Fallback safe: allow if profile missing
		f.allowedCount.Add(1)
		return true
	}

	// 3. Check Includes
	if len(prof.Includes) > 0 {
		matchedInclude := false
		for _, re := range prof.Includes {
			if re.MatchString(toolName) {
				matchedInclude = true
				break
			}
		}
		if !matchedInclude {
			f.filteredCount.Add(1)
			return false
		}
	}

	// 4. Check Excludes (Exclusions strictly override Inclusions)
	for _, re := range prof.Excludes {
		if re.MatchString(toolName) {
			f.filteredCount.Add(1)
			return false
		}
	}

	f.allowedCount.Add(1)
	return true
}

// SetProfile dynamically switches the active profile.
func (f *ToolFilter) SetProfile(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.profiles[name]; !ok {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}

	f.activeProfile = name
	f.profileSwitches.Add(1)
	return nil
}

// CurrentProfile returns the name of the currently active profile.
func (f *ToolFilter) CurrentProfile() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.activeProfile
}

// AvailableProfiles returns a sorted list of registered profile names.
func (f *ToolFilter) AvailableProfiles() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	names := make([]string, 0, len(f.profiles))
	for name := range f.profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AddProfile registers or updates a custom profile at runtime.
func (f *ToolFilter) AddProfile(prof ProfileConfig) error {
	if prof.Name == "" {
		return ErrEmptyProfileName
	}

	compiled, err := f.compileProfile(prof)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[prof.Name] = compiled
	return nil
}

// Stats returns a snapshot of filter evaluation metrics.
func (f *ToolFilter) Stats() FilterStats {
	return FilterStats{
		TotalEvaluations: f.totalEvals.Load(),
		AllowedCount:     f.allowedCount.Load(),
		FilteredCount:    f.filteredCount.Load(),
		ProfileSwitches:  f.profileSwitches.Load(),
	}
}

// RegexCache provides thread-safe, bounded compilation of globs and regular expressions.
type RegexCache struct {
	mu       sync.RWMutex
	cache    map[string]*regexp.Regexp
	capacity int
	hits     atomic.Uint64
	misses   atomic.Uint64
}

// NewRegexCache creates an initialized RegexCache.
func NewRegexCache(capacity int) *RegexCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &RegexCache{
		cache:    make(map[string]*regexp.Regexp, capacity),
		capacity: capacity,
	}
}

// Warmup pre-compiles patterns to eliminate compilation latency on hot paths.
func (c *RegexCache) Warmup(patterns []string, caseInsensitive bool) error {
	for _, p := range patterns {
		if _, err := c.GetOrCompile(p, caseInsensitive); err != nil {
			return err
		}
	}
	return nil
}

// GetOrCompile retrieves a compiled regex from cache or compiles it.
func (c *RegexCache) GetOrCompile(pattern string, caseInsensitive bool) (*regexp.Regexp, error) {
	key := fmt.Sprintf("%t:%s", caseInsensitive, pattern)

	c.mu.RLock()
	re, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
		return re, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-checked locking
	if re, ok := c.cache[key]; ok {
		c.hits.Add(1)
		return re, nil
	}

	regexStr := GlobToRegex(pattern)
	if caseInsensitive {
		regexStr = "(?i)" + regexStr
	}

	compiled, err := regexp.Compile(regexStr)
	if err != nil {
		return nil, fmt.Errorf("failed to compile pattern %q as regex %q: %w", pattern, regexStr, err)
	}

	// Guard bounded capacity
	if len(c.cache) >= c.capacity {
		c.cache = make(map[string]*regexp.Regexp, c.capacity)
	}

	c.cache[key] = compiled
	c.misses.Add(1)
	return compiled, nil
}

// Hits returns the total cache hits.
func (c *RegexCache) Hits() uint64 {
	return c.hits.Load()
}

// Misses returns the total cache misses.
func (c *RegexCache) Misses() uint64 {
	return c.misses.Load()
}

// GlobToRegex converts glob expressions to standard anchored regex expressions.
func GlobToRegex(pattern string) string {
	// If explicitly marked as regex, strip prefix and return as-is
	if strings.HasPrefix(pattern, "regex:") {
		return strings.TrimPrefix(pattern, "regex:")
	}

	var sb strings.Builder
	sb.WriteByte('^')
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		case '.', '+', '(', ')', '{', '}', '^', '$', '|', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(ch)
		default:
			sb.WriteByte(ch)
		}
	}
	sb.WriteByte('$')
	return sb.String()
}
