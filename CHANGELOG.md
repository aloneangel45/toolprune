# Changelog

All notable changes to **ToolPrune** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.3.0] - 2026-11-14

### Added
- **Multi-IDE MCP Preset Expansion (`pkg/preset`, `cmd/preset`, `examples/`)**: Extended `SupportedClients()`, `ParseClient()`, `DefaultPath()`, `Generate()`, `MergeInto()`, `WriteFile()`, `Validate()`, `ValidateForClient()`, and `ValidateFile()` to support **VS Code** (`.vscode/mcp.json` using official `"servers"` + `"type": "stdio"|"sse"` schema while preserving `"inputs"` and per-server extras), **Claude Code** (`.mcp.json` using `"mcpServers"` + `"type": "stdio"|"sse"`), and **Zed** (`%APPDATA%\Zed\settings.json` on Windows or `~/.config/zed/settings.json` using `"context_servers"` -> `"command": {"path", "args", "env"}` while strictly preserving all existing Zed `settings.json` editor settings), alongside Cursor and Windsurf. Added ready-to-use `examples/vscode/mcp.json` and `examples/claude-code/.mcp.json` templates.
- **`toolprune doctor` Diagnostic Subcommand (`cmd/toolprune/doctor.go`, `pkg/doctor`)**: Added `toolprune doctor [--json] [--dir <project_dir>]` to run a strictly read-only local MCP environment and configuration health check:
  - Verifies `toolprune` binary build metadata (`version`, `commit`, `goVersion`, `os/arch`) and `PATH` discoverability.
  - Validates `toolprune.yaml` syntax/semantics (when present) and verifies every active `stdio` upstream executable exists on `PATH` and every `sse` / `streamable-http` upstream carries a valid HTTP/HTTPS URL.
  - Scans project and user MCP client configs (`.cursor/mcp.json`, `.vscode/mcp.json`, `.mcp.json`, Windsurf, Claude Desktop, Zed) without modifying disk, reporting wrapped vs unwrapped server counts and actionable `toolprune preset generate` / `toolprune config patch-claude` commands.
  - Supports `--json` for structured CI/automation pipelines and exits non-zero only when an existing `toolprune.yaml` or configured upstream is broken.
- **Explicit `streamable-http` Upstream Transport Alias (`pkg/config`, `cmd/toolprune`)**: Accepted `transport: "streamable-http"` alongside `transport: "sse"` in `toolprune.yaml` upstream definitions and route/doctor inspectors.

## [1.2.0] - 2026-11-10

### Added
- **RFC #1 Progressive Token-Budget Compactor (`pkg/pruner/budget.go`)**: Sub-0.5ms 5-pass AST compactor (`--max-schema-tokens=N`, `TOOLPRUNE_MAX_SCHEMA_TOKENS`, `pruning.max_schema_tokens`) that strictly retains JSON Schema types, `properties`, `required`, `enum`, `const`, `items`, `$ref`, reachable `$defs`/`definitions`, `anyOf`/`oneOf`/`allOf`, numeric/array bounds, and `additionalProperties` while progressively compacting leaf docstrings and projecting overflow cold tools via `ProjectCompactTool`.
- **Adaptive JSON-RPC Stream Filter (`pkg/proxy/stream_filter.go`)**: Lock-guarded `<5us` call-context recency and domain-prefix affinity tracker, FNV-1a fast-path `tools/list` projection cache (`<0.05ms` cache hit), and `<0.1ms` recursive pre-binding `tools/call` validator returning self-correcting `CallToolResult{IsError: true}` diagnostics before forwarding invalid calls upstream.
- **Multi-Upstream Gateway & MCP Streamable HTTP (`pkg/router/gateway.go`, `pkg/proxy/sse.go`, `pkg/proxy/sse_client.go`)**: Full asynchronous multi-upstream routing over stdio, classic SSE (`GET /sse` + `POST /messages`), and MCP Streamable HTTP (`POST/GET/DELETE /mcp` with `Mcp-Session-Id`), plus outbound `SSEClientTransport` for remote `transport: "sse"` upstreams, paginated `NextCursor` handshakes, and all four dynamic meta-tools (`toolprune_search_tools`, `toolprune_get_tool_schema`, `toolprune_activate_tools`, `toolprune_invoke`) in both gateway and single-upstream wrapper modes.
- **Complete CLI & Preset Workflows**: `--preset` (`fast|balanced|strict|dynamic`), `--stats`, `--profile`, `--namespace` on `toolprune proxy`; `--format csv`, `--verbose` per-parameter breakdown, `--max-schema-tokens`, and live subprocess benchmarking (`toolprune bench -- <cmd>`) on `toolprune bench`; real `--profile` and `--filter` evaluation in `toolprune routes`; and `toolprune config patch-claude` with `--dry-run` and `.bak` backup protection.
- **Realistic Enterprise MCP Benchmark Suite (`pkg/benchmark/suites.go`, `test/fixtures/mcp_suite_realistic.json`)**: Verbose PostgreSQL DDL migration, Filesystem multi-hunk diff, and GitHub AST schemas achieving up to 96.5% token reduction (`cl100k_base`) and `<0.5ms` stream compaction latency.

### Fixed
- **Full JSON Schema & Modern MCP Fidelity**: Preserved `$defs`, `definitions`, `anyOf`, `oneOf`, `allOf`, `not`, `patternProperties`, `dependentSchemas`, `dependentRequired`, `minProperties`, and `maxProperties` on `mcp.InputSchema`, plus `title`, `outputSchema`, `annotations` (`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`, `title`), and `icons` on `mcp.Tool`.
- **UTF-8 Rune Safety & Structural Cloning**: Eliminated slice bounds panics and multi-byte UTF-8 truncation corruption across `params.go`, `condense.go`, `catalog.go`, and `benchmark/table.go`; replaced JSON round-trip cloning in `DeepCopyTools` with direct structural Go cloning.
- **Bounded Framing, Supervisor Drain & Concurrency Races**: Replaced unbounded `ReadBytes`/`ReadString` with `ReadBoundedLine` (`64 KiB` initial buffer, `10 MiB` pre-allocation hard cap); drained `stdoutPipe`/`stderrPipe` completely prior to `cmd.Wait()` in `Supervisor`; resolved data races in `Proxy.SetPruner`, `benchmark.ResetEncoder`, and `Multiplexer.SetMessageHandler`; and added non-crashing JSON-RPC `-32700`/`-32600` recovery for malformed JSON and batch arrays on stdio.
- **Localhost HTTP Security & Namespaced Profiles**: Enforced localhost `Host` and `Origin` validation on `SSEServer` to block DNS-rebinding and cross-origin browser attacks, echoed single matched CORS origins with `Vary: Origin`, enforced `Filter.IsAllowed` inside meta-tools, and added `*__get_*`, `*__list_*`, `*__read_*`, `*__search_*`, and `*__fetch_*` to `ProfileMinimal`.
- **Installers & CI/CD Pipeline**: Fixed `$Arch` archive naming in `install.ps1`, added `checksums.txt` SHA-256 verification to `install.sh` and `install.ps1`, added `-race` testing on Linux/macOS in `ci.yml`, added `windows/arm64` + pre-release test gate in `release.yml`, and injected version `-X` ldflags in `Dockerfile`.

## [1.1.0] - 2026-11-08

### Added
- **IDE Preset Generator & Validator (`pkg/preset`, `cmd/preset`)**: Added `toolprune preset generate` and `toolprune preset validate` for Cursor (`.cursor/mcp.json`) and Windsurf (`mcp_config.json`) configurations.
- **Declarative Multi-Upstream Configuration (`pkg/config`)**: Added `toolprune.yaml` schema validation, `${VAR:-default}` environment variable expansion, `TOOLPRUNE_*` environment overrides, and `toolprune routes` / `toolprune config init|validate` subcommands.
- **Dynamic Catalog Windowing Foundations (`pkg/router`)**: Added LRU active tool windowing, `SchemaStore` raw schema registry, glob-based security profiles (`full`, `read_only`, `dev`, `minimal`), and debounced `notifications/tools/list_changed` coalescing.

## [1.0.0] - 2026-11-05

### Added
- High-performance transparent MCP proxy engine written in Go.
- Dynamic JSON-RPC schema pruning reducing LLM token consumption up to 91%.
- Low-allocation hot path stream filter for stdio and SSE transport protocols.
- Native integration profiles for Claude Code, Cursor, and Windsurf IDEs.
- Automated CI pipeline with multi-platform cross-compilation binaries (Linux, macOS, Windows).
- Deterministic token benchmark test suite and metrics exporter.
