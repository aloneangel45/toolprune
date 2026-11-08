# ToolPrune client config presets

This directory contains ready-to-use MCP client configuration presets that wire
AI clients to a ToolPrune proxy instance.

## Cursor

Copy `cursor/mcp.json` to your project's `.cursor/mcp.json`:

    Copy-Item examples\cursor\mcp.json .cursor\mcp.json

Or generate it in place:

    toolprune preset generate --client cursor --output .cursor\mcp.json --config toolprune.yaml --merge

## Windsurf (Codeium)

Copy `windsurf/mcp_config.json` to your global Windsurf config
`~/.codeium/windsurf/mcp_config.json`:

    Copy-Item examples\windsurf\mcp_config.json $HOME\.codeium\windsurf\mcp_config.json

Or generate it in place:

    toolprune preset generate --client windsurf --config toolprune.yaml --merge

Both presets register a single `toolprune` stdio MCP server that launches
`toolprune proxy --config toolprune.yaml`, so the client talks to ToolPrune,
which multiplexes and prunes the upstream servers defined in `toolprune.yaml`.

Validate any generated or hand-edited file against the client schema:

    toolprune preset validate .cursor\mcp.json