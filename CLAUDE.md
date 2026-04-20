# codeindex — Go Code Indexer

This project is a Go codebase indexer using tree-sitter for parsing and BoltDB (B+ tree) for storage.

## Before exploring any Go codebase

Always index it first:
```
codeindex index /path/to/project
```

Or set `CODEINDEX_DB` to point at an existing index:
```
export CODEINDEX_DB=/path/to/project/.codeindex.bolt
```

## Commands to use instead of grep

| Instead of | Use |
|---|---|
| `grep -r "func NewUser"` | `codeindex find NewUser` |
| `grep -r "\.ServeHTTP("` | `codeindex usages ServeHTTP` |
| `grep -r "type.*struct"` | `codeindex list struct` |
| `grep -r "RoleAdmin"` | `codeindex usages RoleAdmin` |

## All commands

```bash
codeindex index [dir]              # build the index (run this first)
codeindex find <name>              # where is this symbol defined?
codeindex usages <name>            # where is this called / referenced?
codeindex search <pattern> [kind]  # search by name (kind: func|method|struct|interface|type|var|const)
codeindex list [kind]              # list all symbols of a kind
codeindex calls-from <Recv.Method> # what does this function call?
codeindex stats                    # index metadata
```

## Storage

Index is stored in `.codeindex.bolt` (BoltDB B+ tree).
- `find` / `usages` / `calls-from` → O(log n) B+ tree lookup
- `list` / `search` → cursor scan with kind-prefix index
- Override path: `CODEINDEX_DB=/other/path codeindex ...`
