# codeindex

Go codebase indexer for AI-assisted development. Uses tree-sitter for parsing, BoltDB for symbol storage, and SQLite FTS5 for full-text search. Integrates with Claude Code via MCP — Claude gets native tools to find symbols, trace usages, and search code without reading every file.

## Requirements

- Go 1.21+
- A C compiler (gcc on Linux, Xcode CLT on Mac: `xcode-select --install`, MinGW on Windows)

## Install

```bash
go install github.com/ddbera95/codeindex@latest
```

Make sure `$GOPATH/bin` (usually `~/go/bin`) is in your `$PATH`.

## Setup (run once per project)

```bash
cd your-project
codeindex init    # creates .codeindex/, registers MCP server in .claude/settings.json
codeindex index . # build the index
```

Then open Claude Code in the project — the MCP server starts automatically.

## Commands

```bash
codeindex init                     # initialize in current project (run once)
codeindex index [dir]              # build symbol + FTS index
codeindex find <name>              # find symbol definition + signature
codeindex usages <name>            # find all call sites
codeindex search <pattern> [kind]  # search by name (kind: func|method|struct|interface|type|var|const)
codeindex list [kind]              # list all symbols
codeindex calls-from <Recv.Method> # show what a function calls
codeindex fts <phrase>             # full-text search ("exact phrase", word*, word OR word)
codeindex fts-update [dir]         # incremental FTS update
codeindex stats                    # index metadata
```

## Claude Code integration

`codeindex init` registers an MCP server in `.claude/settings.json`. Claude Code then has these native tools:

| Tool | Description |
|---|---|
| `find_symbol` | Definition + signature + return type |
| `find_usages` | All call sites across the codebase |
| `search_symbols` | Pattern search with kind filter |
| `full_text_search` | FTS5 search across source files |
| `calls_from` | Call graph of a function/method |
| `get_config` | Show current config |
| `update_config` | Add/remove FTS file patterns |
| `get_stats` | Index statistics |
| `rebuild_index` | Rebuild symbol index |
| `update_fts` | Incremental FTS refresh |

The FTS index updates automatically when files change (file watcher runs inside the MCP server).

## Config

Edit `.codeindex/config.json` to add more file types to FTS:

```json
{
  "fts": {
    "include": ["*.go", "*.md", "*.yaml"],
    "excludeDirs": ["vendor", ".git", "node_modules", "testdata", ".codeindex"],
    "maxFileSizeKB": 50
  }
}
```

Or ask Claude to do it: *"add markdown files to the FTS index"* — it will call `update_config` directly.

## What gets indexed

- **Symbol index** (BoltDB): Go files only — functions, methods, structs, interfaces, types, vars, constants, and their references
- **FTS index** (SQLite FTS5): Go files by default, configurable via `config.json`

## Keeping the index fresh

`codeindex init` installs a git pre-commit hook that rebuilds the index on every commit. For uncommitted changes, run `codeindex index .` manually or ask Claude to call `rebuild_index`.
