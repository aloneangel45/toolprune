# ToolPrune client config presets

This directory contains ready-to-use MCP client configuration presets that wire
AI clients to a ToolPrune proxy instance.

## Cursor

Copy `cursor/mcp.json` to your project's `.cursor/mcp.json`:

    Copy-Item examples\cursor\mcp.json .cursor\mcp.json

Or generate it in place:

    toolprune preset generate --client cursor --output .cursor\mcp.json --config toolprune.yaml --merge

## VS Code (GitHub Copilot MCP)

Copy `vscode/mcp.json` to your project's `.vscode/mcp.json`:

    Copy-Item examples\vscode\mcp.json .vscode\mcp.json

Or generate/merge it in place (preserving existing `"inputs"` and sibling servers):

    toolprune preset generate --client vscode --output .vscode\mcp.json --config toolprune.yaml --merge

## Claude Code

Copy `claude-code/.mcp.json` to your project root `.mcp.json`:

    Copy-Item examples\claude-code\.mcp.json .mcp.json

Or generate/merge it in place:

    toolprune preset generate --client claude-code --output .mcp.json --config toolprune.yaml --merge

## Windsurf (Codeium)

Copy `windsurf/mcp_config.json` to your global Windsurf config
`~/.codeium/windsurf/mcp_config.json`:

    Copy-Item examples\windsurf\mcp_config.json $HOME\.codeium\windsurf\mcp_config.json

Or generate it in place:

    toolprune preset generate --client windsurf --config toolprune.yaml --merge

## Zed

Merge a ToolPrune `"context_servers"` entry into your Zed `settings.json`
(`~/.config/zed/settings.json` on macOS/Linux, `%APPDATA%\Zed\settings.json` on Windows)
without touching any other Zed editor settings:

    toolprune preset generate --client zed --config toolprune.yaml --merge

## Validation

All presets register a `toolprune` MCP server that launches
`toolprune proxy --config toolprune.yaml`, so the client talks to ToolPrune,
which multiplexes and prunes the upstream servers defined in `toolprune.yaml`.

Validate any generated or hand-edited file against the client schema:

    toolprune preset validate .cursor\mcp.json
    toolprune preset validate .vscode\mcp.json
    toolprune preset validate .mcp.json
