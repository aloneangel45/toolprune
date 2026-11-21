package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

const (
	// LockfileVersion identifies the toolprune.lock schema version.
	LockfileVersion = "1"
	// DefaultLockfileName is the standard lockfile path.
	DefaultLockfileName = "toolprune.lock"
)

// LockedTool captures the pinned cryptographic digest of a single MCP tool definition.
type LockedTool struct {
	SHA256      string `json:"sha256"`
	Description string `json:"description,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

// Lockfile persists canonical SHA-256 hashes of MCP tool schemas per upstream server
// to detect mid-session or cross-restart rug-pull mutations.
type Lockfile struct {
	Version   string                           `json:"version"`
	UpdatedAt string                           `json:"updatedAt"`
	Upstreams map[string]map[string]LockedTool `json:"upstreams"`
}

// NewLockfile creates an empty initialized Lockfile.
func NewLockfile() *Lockfile {
	return &Lockfile{
		Version:   LockfileVersion,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Upstreams: make(map[string]map[string]LockedTool),
	}
}

// canonicalToolEnvelope isolates the canonical fields hashed for rug-pull detection.
type canonicalToolEnvelope struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema mcp.InputSchema `json:"inputSchema"`
}

// ComputeToolHash computes the deterministic canonical SHA-256 hex digest of
// a tool's (name, description, inputSchema). Go's encoding/json deterministically
// sorts map keys, so structurally identical schemas produce identical digests.
func ComputeToolHash(tool mcp.Tool) string {
	env := canonicalToolEnvelope{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: tool.InputSchema,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		// Fallback to deterministic string hash if schema contains unexpected values
		sum := sha256.Sum256([]byte(tool.Name + "\x00" + tool.Description))
		return hex.EncodeToString(sum[:])
	}
	// Normalize via generic round-trip so any nested maps/numbers serialize canonically.
	var generic any
	if err := json.Unmarshal(raw, &generic); err == nil {
		if canon, err := json.Marshal(generic); err == nil {
			sum := sha256.Sum256(canon)
			return hex.EncodeToString(sum[:])
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// LoadLockfile reads and parses a toolprune.lock JSON file from disk.
func LoadLockfile(path string) (*Lockfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("failed to parse lockfile %q: %w", path, err)
	}
	if lf.Version == "" {
		lf.Version = LockfileVersion
	}
	if lf.Upstreams == nil {
		lf.Upstreams = make(map[string]map[string]LockedTool)
	}
	return &lf, nil
}

// SaveLockfile atomically persists the lockfile to path as deterministic indented JSON.
func SaveLockfile(path string, lf *Lockfile) error {
	if lf == nil {
		return fmt.Errorf("lockfile cannot be nil")
	}
	if lf.Version == "" {
		lf.Version = LockfileVersion
	}
	lf.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if lf.Upstreams == nil {
		lf.Upstreams = make(map[string]map[string]LockedTool)
	}

	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal lockfile: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create lockfile directory %q: %w", dir, err)
		}
	}
	if dir == "" {
		dir = "."
	}

	tmp, err := os.CreateTemp(dir, ".toolprune-lock-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary lockfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		// On Windows, replacing an existing file via os.Rename can occasionally fail if held;
		// remove and retry once cleanly.
		if runtime.GOOS == "windows" {
			_ = os.Remove(path)
			if rErr := os.Rename(tmpPath, path); rErr != nil {
				return fmt.Errorf("failed to write lockfile %q: %w", path, rErr)
			}
		} else {
			return fmt.Errorf("failed to write lockfile %q: %w", path, err)
		}
	}
	cleanup = false
	return nil
}

// Update records or refreshes the canonical SHA-256 hashes for all tools of an upstream.
func (lf *Lockfile) Update(upstream string, tools []mcp.Tool) {
	if lf.Upstreams == nil {
		lf.Upstreams = make(map[string]map[string]LockedTool)
	}
	if upstream == "" {
		upstream = "default"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	entry := make(map[string]LockedTool, len(tools))
	for _, t := range tools {
		entry[t.Name] = LockedTool{
			SHA256:      ComputeToolHash(t),
			Description: truncatePreview(t.Description, 80),
			UpdatedAt:   now,
		}
	}
	lf.Upstreams[upstream] = entry
	lf.UpdatedAt = now
}

// Verify compares live tools for an upstream against pinned hashes in the lockfile.
// Returns findings for any drifted (hash mismatch) or newly injected unpinned tools.
func (lf *Lockfile) Verify(upstream string, tools []mcp.Tool) []Finding {
	if lf == nil || lf.Upstreams == nil {
		return nil
	}
	if upstream == "" {
		upstream = "default"
	}
	pinned, exists := lf.Upstreams[upstream]
	if !exists || len(pinned) == 0 {
		return nil
	}

	var findings []Finding
	sorted := append([]mcp.Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	for _, t := range sorted {
		gotHash := ComputeToolHash(t)
		locked, ok := pinned[t.Name]
		if !ok {
			findings = append(findings, Finding{
				RuleID:      RuleRugPullUnpinned,
				RuleName:    "UnpinnedToolAdded",
				Category:    CategoryRugPull,
				Severity:    SeverityMedium,
				ToolName:    t.Name,
				Upstream:    upstream,
				Field:       "tool",
				Message:     fmt.Sprintf("tool %q from upstream %q is not pinned in toolprune.lock (sha256=%s)", t.Name, upstream, shortHash(gotHash)),
				Evidence:    fmt.Sprintf("observed=%s", gotHash),
				Remediation: "Inspect the new tool definition and run ToolPrune with security.mode=update or 'toolprune audit --update-lockfile' if trusted.",
			})
			continue
		}
		if !strings.EqualFold(locked.SHA256, gotHash) {
			findings = append(findings, Finding{
				RuleID:      RuleRugPullLockfileDrift,
				RuleName:    "LockfileSchemaDrift",
				Category:    CategoryRugPull,
				Severity:    SeverityCritical,
				ToolName:    t.Name,
				Upstream:    upstream,
				Field:       "schema",
				Message:     fmt.Sprintf("possible rug-pull: tool %q from upstream %q drifted from toolprune.lock (expected %s, got %s)", t.Name, upstream, shortHash(locked.SHA256), shortHash(gotHash)),
				Evidence:    fmt.Sprintf("expected=%s observed=%s", locked.SHA256, gotHash),
				Remediation: "Verify whether the upstream server was legitimately upgraded; quarantine tool until schema diff is reviewed.",
			})
		}
	}
	return findings
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func truncatePreview(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}
