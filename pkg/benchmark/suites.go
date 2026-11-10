package benchmark

import (
	"encoding/json"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// RealisticEnterpriseSuite builds a representative verbose enterprise MCP tool suite
// modeling PostgreSQL (multi-step DDL migration AST with $defs/$ref and anyOf),
// Filesystem (multi-hunk unified diff AST), and GitHub (PR review and Git tree ASTs).
func RealisticEnterpriseSuite() []mcp.Tool {
	readOnlyFalse := false
	readOnlyTrue := true
	destructiveTrue := true
	destructiveFalse := false
	idempotentTrue := true
	minOne := 1.0
	maxHundred := 100.0
	minZero := 0.0
	maxContextLines := 20.0
	minHunks := 1
	maxHunks := 200

	return []mcp.Tool{
		{
			Name:  "postgres__apply_ddl_migration",
			Title: "PostgreSQL Transactional DDL Schema Migration Executor",
			Description: `Executes a transactional multi-step PostgreSQL Data Definition Language (DDL) migration plan with advisory lock coordination, lock_timeout enforcement, concurrent index safety checks, and optional dry-run EXPLAIN/rollback verification.

### Operational Guarantees & Locking Semantics
When executing schema alterations against high-throughput OLTP tables in PostgreSQL 14+, naive ALTER TABLE statements acquire ACCESS EXCLUSIVE locks that stall active read/write queries if queued behind long-running transactions. This tool wraps every migration batch in an explicit session guard configuring lock_timeout, statement_timeout, and idle_in_transaction_session_timeout prior to acquiring pg_try_advisory_xact_lock.

### Usage Example
` + "```json\n" + `{
  "migration_id": "20261110_add_audit_events",
  "database": "prod_ledger",
  "lock_timeout_ms": 2500,
  "operations": [
    {
      "op_type": "create_table",
      "table_name": "audit_events",
      "schema_name": "public",
      "columns": [
        {"name": "id", "data_type": "uuid", "nullable": false, "primary_key": true},
        {"name": "actor_id", "data_type": "text", "nullable": false}
      ]
    }
  ]
}
` + "```",
			Annotations: &mcp.ToolAnnotations{
				Title:           "Apply PostgreSQL DDL Migration",
				ReadOnlyHint:    &readOnlyFalse,
				DestructiveHint: &destructiveTrue,
				IdempotentHint:  &idempotentTrue,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Defs: map[string]any{
					"ColumnSpec": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"description":          "Declarative PostgreSQL column specification defining SQL type, nullability, default expression, generated identity rules, and foreign key references.",
						"required":             []string{"name", "data_type"},
						"properties": map[string]any{
							"name": map[string]any{
								"type":        "string",
								"description": "Unquoted PostgreSQL identifier for the target column (must match snake_case convention and avoid reserved SQL keywords).",
							},
							"data_type": map[string]any{
								"type":        "string",
								"description": "Canonical PostgreSQL column data type expression including optional precision or array bounds.",
								"enum": []string{
									"uuid", "text", "varchar", "integer", "bigint", "numeric",
									"boolean", "timestamptz", "jsonb", "bytea", "inet", "vector",
								},
							},
							"nullable": map[string]any{
								"type":        "boolean",
								"description": "Whether the column permits SQL NULL values. Defaults to true unless primary_key is enabled.",
							},
							"primary_key": map[string]any{
								"type":        "boolean",
								"description": "Marks this column as part of the table PRIMARY KEY btree index constraint.",
							},
							"default_sql": map[string]any{
								"type":        "string",
								"description": "Deterministic SQL expression evaluated for omitted column values (for example 'gen_random_uuid()' or 'now()').",
							},
							"check_expr": map[string]any{
								"type":        "string",
								"description": "Optional boolean SQL predicate enforced as a row-level CHECK constraint on writes.",
							},
						},
					},
					"IndexSpec": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"description":          "PostgreSQL secondary index definition supporting btree, gin, gist, brin, and hnsw access methods with partial WHERE predicates.",
						"required":             []string{"index_name", "columns"},
						"properties": map[string]any{
							"index_name": map[string]any{
								"type":        "string",
								"description": "Deterministic relation identifier for the index within the target schema namespace.",
							},
							"method": map[string]any{
								"type":        "string",
								"description": "PostgreSQL index access method controlling storage layout and operator class support.",
								"enum":        []string{"btree", "gin", "gist", "hash", "brin", "hnsw"},
							},
							"columns": map[string]any{
								"type":        "array",
								"description": "Ordered list of key columns or functional index expressions included in the index key.",
								"minItems":    1,
								"items": map[string]any{
									"type":        "string",
									"description": "Column name or parenthesized SQL key expression.",
								},
							},
							"unique": map[string]any{
								"type":        "boolean",
								"description": "Enforces uniqueness across indexed non-null key tuples.",
							},
							"where_predicate": map[string]any{
								"type":        "string",
								"description": "Optional partial index filter predicate reducing index size and write amplification.",
							},
						},
					},
				},
				Required: []string{"migration_id", "database", "operations"},
				Properties: map[string]any{
					"migration_id": map[string]any{
						"type":        "string",
						"description": "Globally unique monotonically ordered migration identifier recorded in schema_migrations ledger.",
					},
					"database": map[string]any{
						"type":        "string",
						"description": "Target logical PostgreSQL database name configured in the upstream connection pool.",
					},
					"lock_timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Maximum duration in milliseconds to wait for table or row locks before aborting safely without blocking traffic.",
						"minimum":     50,
						"maximum":     30000,
					},
					"statement_timeout_ms": map[string]any{
						"type":        "integer",
						"description": "Hard execution ceiling in milliseconds applied to each DDL statement inside the migration transaction.",
						"minimum":     100,
						"maximum":     600000,
					},
					"dry_run": map[string]any{
						"type":        "boolean",
						"description": "Executes all DDL steps inside a savepoint and issues ROLLBACK at completion to validate catalog compatibility without persisting changes.",
					},
					"operations": map[string]any{
						"type":        "array",
						"description": "Ordered sequence of atomic DDL AST operations executed sequentially within the migration envelope.",
						"minItems":    minHunks,
						"maxItems":    maxHunks,
						"items": map[string]any{
							"type":        "object",
							"description": "Single PostgreSQL DDL AST operation node.",
							"required":    []string{"op_type", "table_name"},
							"properties": map[string]any{
								"op_type": map[string]any{
									"type":        "string",
									"description": "DDL operation discriminator specifying the catalog transformation.",
									"enum":        []string{"create_table", "alter_table_add_column", "alter_table_drop_column", "create_index", "drop_index"},
								},
								"schema_name": map[string]any{
									"type":        "string",
									"description": "PostgreSQL schema namespace containing the target relation (defaults to 'public').",
								},
								"table_name": map[string]any{
									"type":        "string",
									"description": "Target PostgreSQL relation name for the DDL statement.",
								},
								"columns": map[string]any{
									"type":        "array",
									"description": "Column specifications referenced when op_type creates or alters table columns.",
									"items": map[string]any{
										"$ref": "#/$defs/ColumnSpec",
									},
								},
								"indexes": map[string]any{
									"type":        "array",
									"description": "Secondary index specifications created alongside or on top of the target relation.",
									"items": map[string]any{
										"$ref": "#/$defs/IndexSpec",
									},
								},
							},
						},
					},
				},
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"applied": map[string]any{
						"type":        "boolean",
						"description": "Indicates whether the migration transaction committed permanently to the catalog.",
					},
					"elapsed_ms": map[string]any{
						"type":        "number",
						"description": "Total wall-clock execution duration of the migration transaction in milliseconds.",
					},
				},
				"required": []any{"applied", "elapsed_ms"},
			},
		},
		{
			Name:  "postgres__analyze_query_plan",
			Title: "PostgreSQL EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) Inspector",
			Description: `Runs PostgreSQL EXPLAIN with configurable instrumentation flags (BUFFERS, WAL, TIMING, SETTINGS, FORMAT JSON) inside a read-only transaction to diagnose sequential scan regressions, nested loop row misestimates, heap fetches, and shared buffer cache misses.

### Diagnostic Output
Returns structured plan tree nodes annotated with actual vs estimated row cardinalities, shared/local/temp block hits and dirtied pages, sort spill memory usage, and index condition pushdowns.`,
			Annotations: &mcp.ToolAnnotations{
				Title:           "Analyze PostgreSQL Query Plan",
				ReadOnlyHint:    &readOnlyTrue,
				DestructiveHint: &destructiveFalse,
				IdempotentHint:  &idempotentTrue,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Required:             []string{"database", "sql_query"},
				Properties: map[string]any{
					"database": map[string]any{
						"type":        "string",
						"description": "Logical PostgreSQL database target against which the query planner will run.",
					},
					"sql_query": map[string]any{
						"type":        "string",
						"description": "Parameterized SQL SELECT or CTE query string to inspect via EXPLAIN.",
					},
					"parameters": map[string]any{
						"type":        "array",
						"description": "Positional bind parameter values ($1, $2, ...) substituted safely via prepared statement protocol.",
						"items": map[string]any{
							"anyOf": []any{
								map[string]any{"type": "string", "description": "Textual or UUID bind parameter value."},
								map[string]any{"type": "number", "description": "Numeric scalar bind parameter value."},
								map[string]any{"type": "boolean", "description": "Boolean predicate bind parameter value."},
							},
						},
					},
					"execute_analyze": map[string]any{
						"type":        "boolean",
						"description": "When true, executes the query inside a read-only ROLLBACK wrapper to collect actual runtime row counts and buffer statistics.",
					},
					"include_buffers": map[string]any{
						"type":        "boolean",
						"description": "Collects shared, local, and temp block read/hit/write counters per plan node.",
					},
					"max_rows_sampled": map[string]any{
						"type":        "integer",
						"description": "Optional row limit wrapper applied during sampling to cap result materialization.",
						"minimum":     minOne,
						"maximum":     maxHundred,
					},
				},
			},
		},
		{
			Name:  "filesystem__apply_multi_hunk_diff",
			Title: "Atomic Multi-File Unified Diff & AST Patch Engine",
			Description: `Applies structured multi-file, multi-hunk unified patches atomically across workspace source files with cryptographic SHA-256 pre-image verification, configurable context-line fuzz matching, line-ending preservation (LF vs CRLF), and full rollback if any single hunk fails verification.

### Patch Resolution Rules
1. Every target file path is canonicalized and verified to reside strictly beneath the workspace root without escaping via symlinks.
2. Each hunk locates its anchor region using exact line bounds or surrounding context lines before splicing replacement lines.
3. In dry_run mode, computes unified diff previews and post-patch SHA-256 digests without mutating disk files.`,
			Annotations: &mcp.ToolAnnotations{
				Title:           "Apply Multi-Hunk Workspace Patch",
				ReadOnlyHint:    &readOnlyFalse,
				DestructiveHint: &destructiveFalse,
				IdempotentHint:  &idempotentTrue,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Defs: map[string]any{
					"DiffHunk": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"description":          "Contiguous source hunk specifying exact search lines, replacement lines, and optional surrounding context anchors.",
						"required":             []string{"old_text", "new_text"},
						"properties": map[string]any{
							"start_line": map[string]any{
								"type":        "integer",
								"description": "1-based starting line hint used to disambiguate identical code blocks appearing multiple times in the same file.",
								"minimum":     1,
							},
							"old_text": map[string]any{
								"type":        "string",
								"description": "Exact contiguous block of existing source text including indentation to locate and replace.",
							},
							"new_text": map[string]any{
								"type":        "string",
								"description": "Replacement source block substituted in place of old_text (empty string deletes the matched lines).",
							},
							"context_before": map[string]any{
								"type":        "string",
								"description": "Optional immutable lines immediately preceding old_text to anchor patch placement.",
							},
							"context_after": map[string]any{
								"type":        "string",
								"description": "Optional immutable lines immediately following old_text to anchor patch placement.",
							},
						},
					},
				},
				Required: []string{"file_patches"},
				Properties: map[string]any{
					"workspace_root": map[string]any{
						"type":        "string",
						"description": "Optional workspace root directory used to resolve relative patch paths.",
					},
					"atomic_rollback": map[string]any{
						"type":        "boolean",
						"description": "Restores all modified files to their exact byte-for-byte original contents if any file patch fails.",
					},
					"dry_run": map[string]any{
						"type":        "boolean",
						"description": "Validates hunk anchors and returns unified diff previews without writing changes to disk.",
					},
					"context_lines": map[string]any{
						"type":        "integer",
						"description": "Number of unchanged surrounding lines rendered in unified diff output summaries.",
						"minimum":     minZero,
						"maximum":     maxContextLines,
					},
					"line_ending_policy": map[string]any{
						"type":        "string",
						"description": "Controls how newline sequences are normalized when writing patched files.",
						"enum":        []string{"preserve", "lf", "crlf"},
					},
					"file_patches": map[string]any{
						"type":        "array",
						"description": "Ordered collection of file-level patch specifications applied in a single atomic batch.",
						"minItems":    1,
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"description":          "Patch specification targeting a single workspace file.",
							"required":             []string{"path", "hunks"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Workspace-relative path to the source file being modified.",
								},
								"expected_sha256": map[string]any{
									"type":        "string",
									"description": "Optional hex-encoded SHA-256 digest of the file prior to patching for optimistic concurrency control.",
								},
								"encoding": map[string]any{
									"type":        "string",
									"description": "Character encoding used when decoding and re-encoding file bytes.",
									"enum":        []string{"utf-8", "utf-16le", "latin1"},
								},
								"hunks": map[string]any{
									"type":        "array",
									"description": "Non-overlapping hunks applied top-to-bottom within this file.",
									"minItems":    1,
									"items": map[string]any{
										"$ref": "#/$defs/DiffHunk",
									},
								},
							},
						},
					},
				},
			},
		},
		{
			Name:        "filesystem__search_workspace_ast",
			Title:       "Structural Regex & Symbol Workspace Code Search",
			Description: `Searches workspace files using compiled RE2 regular expressions, glob include/exclude path filters, binary-file skipping, and surrounding context line extraction for rapid repository navigation.`,
			Annotations: &mcp.ToolAnnotations{
				Title:           "Search Workspace Files",
				ReadOnlyHint:    &readOnlyTrue,
				DestructiveHint: &destructiveFalse,
				IdempotentHint:  &idempotentTrue,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Required:             []string{"query"},
				Properties: map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Literal substring or RE2 regular expression pattern matched against UTF-8 source lines.",
					},
					"is_regex": map[string]any{
						"type":        "boolean",
						"description": "Interprets query as a regular expression rather than a literal string.",
					},
					"case_sensitive": map[string]any{
						"type":        "boolean",
						"description": "Forces case-sensitive matching even when query contains only lowercase characters.",
					},
					"include_globs": map[string]any{
						"type":        "array",
						"description": "Glob patterns restricting search traversal to matching file paths (for example '**/*.go').",
						"items":       map[string]any{"type": "string", "description": "Glob pattern string."},
					},
					"exclude_globs": map[string]any{
						"type":        "array",
						"description": "Glob patterns excluding vendor, build artifact, or generated directories from traversal.",
						"items":       map[string]any{"type": "string", "description": "Exclusion glob pattern string."},
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Upper bound on matched line records returned before truncating output.",
						"minimum":     1,
						"maximum":     500,
					},
				},
			},
		},
		{
			Name:  "github__create_pull_request_review",
			Title: "GitHub Pull Request Multi-Line Code Review Submitter",
			Description: `Submits a comprehensive GitHub Pull Request review containing top-level summary markdown, review verdict state (APPROVE, REQUEST_CHANGES, COMMENT), and inline multi-line diff review comments pinned to specific commit SHA blobs and diff sides.

### GitHub REST & GraphQL Review Constraints
- Inline review comments must reference file paths present in the pull request diff and line numbers belonging to a diff hunk unless subject_type is 'file'.
- When submitting multi-line suggestions, start_line must be strictly less than line and both bounds must reside within the same unified diff hunk.`,
			Annotations: &mcp.ToolAnnotations{
				Title:           "Submit Pull Request Review",
				ReadOnlyHint:    &readOnlyFalse,
				DestructiveHint: &destructiveFalse,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Required:             []string{"owner", "repo", "pull_number", "event"},
				Properties: map[string]any{
					"owner": map[string]any{
						"type":        "string",
						"description": "GitHub organization or user login owning the target repository.",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository name without .git suffix.",
					},
					"pull_number": map[string]any{
						"type":        "integer",
						"description": "Pull request number within the repository.",
						"minimum":     1,
					},
					"commit_id": map[string]any{
						"type":        "string",
						"description": "Optional 40-character Git commit SHA pinning inline review comments to a specific PR head revision.",
					},
					"event": map[string]any{
						"type":        "string",
						"description": "Review transition action applied when publishing the review.",
						"enum":        []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"},
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Top-level GitHub-flavored Markdown review summary required for REQUEST_CHANGES and COMMENT events.",
					},
					"comments": map[string]any{
						"type":        "array",
						"description": "Inline diff comments attached atomically to the pull request review.",
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"description":          "Single inline diff comment anchored to a repository path and diff line range.",
							"required":             []string{"path", "line", "body"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Repository-relative file path being commented on.",
								},
								"line": map[string]any{
									"type":        "integer",
									"description": "End line number in the diff hunk to which the comment applies.",
									"minimum":     1,
								},
								"start_line": map[string]any{
									"type":        "integer",
									"description": "First line number of a multi-line comment range within the same diff hunk.",
									"minimum":     1,
								},
								"side": map[string]any{
									"type":        "string",
									"description": "Diff side targeted by line ('LEFT' for deletions, 'RIGHT' for additions/context).",
									"enum":        []string{"LEFT", "RIGHT"},
								},
								"body": map[string]any{
									"type":        "string",
									"description": "Markdown comment text supporting fenced suggestion blocks.",
								},
							},
						},
					},
				},
			},
		},
		{
			Name:        "github__commit_multi_file_tree",
			Title:       "GitHub Atomic Multi-File Git Tree & Commit Creator",
			Description: `Creates an atomic multi-file Git commit on a remote GitHub branch via the Git Data API (blobs, trees, commits, and ref fast-forward update) without requiring a local repository clone, supporting file additions, updates, executable bit changes, and deletions in a single commit.`,
			Annotations: &mcp.ToolAnnotations{
				Title:           "Commit Multi-File Git Tree",
				ReadOnlyHint:    &readOnlyFalse,
				DestructiveHint: &destructiveFalse,
			},
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Required:             []string{"owner", "repo", "branch", "message", "entries"},
				Properties: map[string]any{
					"owner": map[string]any{
						"type":        "string",
						"description": "GitHub organization or user account login.",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": "Target GitHub repository name.",
					},
					"branch": map[string]any{
						"type":        "string",
						"description": "Target branch name (for example 'feat/stream-filter') to advance with the new commit.",
					},
					"base_branch": map[string]any{
						"type":        "string",
						"description": "Optional base branch from which to create branch if it does not yet exist remotely.",
					},
					"expected_head_oid": map[string]any{
						"type":        "string",
						"description": "Expected 40-character SHA of the current branch tip to prevent non-fast-forward race overwrites.",
					},
					"message": map[string]any{
						"type":        "string",
						"description": "Conventional Commit headline and optional body message.",
					},
					"entries": map[string]any{
						"type":        "array",
						"description": "Git tree file mutations included atomically in the new commit.",
						"minItems":    1,
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"description":          "Single Git tree entry mutation.",
							"required":             []string{"path", "mode"},
							"properties": map[string]any{
								"path": map[string]any{
									"type":        "string",
									"description": "Repository-relative POSIX file path.",
								},
								"mode": map[string]any{
									"type":        "string",
									"description": "Git tree file mode ('100644' regular file, '100755' executable, '120000' symlink).",
									"enum":        []string{"100644", "100755", "120000"},
								},
								"content": map[string]any{
									"type":        "string",
									"description": "UTF-8 file contents to write (omit and set delete=true to remove file from tree).",
								},
								"delete": map[string]any{
									"type":        "boolean",
									"description": "When true, deletes path from the resulting Git tree by setting sha to null.",
								},
							},
						},
					},
				},
			},
		},
	}
}

// MarshalRealisticEnterpriseSuiteJSON serializes RealisticEnterpriseSuite as formatted JSON.
func MarshalRealisticEnterpriseSuiteJSON() ([]byte, error) {
	return json.MarshalIndent(RealisticEnterpriseSuite(), "", "  ")
}
