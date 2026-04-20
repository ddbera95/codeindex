package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ddbera95/codeindex/config"
	"github.com/ddbera95/codeindex/indexer"
	"github.com/ddbera95/codeindex/mcp"
	"github.com/ddbera95/codeindex/store"
)

const (
	ciDir   = ".codeindex"
	dbFile  = ".codeindex/index.bolt"
	ftsFile = ".codeindex/index.fts"
)

func dbPath() string  { return dbFile }
func ftsPath() string { return ftsFile }

func isInitialized() bool {
	_, err := os.Stat(ciDir)
	return err == nil
}

func requireInit() {
	if !isInitialized() {
		fmt.Fprintln(os.Stderr, "error: not initialized — run: codeindex init")
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "init":
		cmdInit()
	case "mcp":
		requireInit()
		cmdMCP()
	case "index":
		requireInit()
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		cmdIndex(dir)
	case "find":
		requireInit()
		needArg("find <name>")
		cmdFind(os.Args[2])
	case "usages":
		requireInit()
		needArg("usages <name>")
		cmdUsages(os.Args[2])
	case "search":
		requireInit()
		needArg("search <pattern> [kind]")
		kind := ""
		if len(os.Args) > 3 {
			kind = os.Args[3]
		}
		cmdSearch(os.Args[2], kind)
	case "list":
		requireInit()
		kind := ""
		if len(os.Args) > 2 {
			kind = os.Args[2]
		}
		cmdList(kind)
	case "calls-from":
		requireInit()
		needArg("calls-from <Receiver.Method>")
		cmdCallsFrom(os.Args[2])
	case "stats":
		requireInit()
		cmdStats()
	case "fts":
		requireInit()
		needArg("fts <phrase>  [tip: quote multi-word phrases]")
		cmdFTS(os.Args[2])
	case "fts-update":
		requireInit()
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		cmdFTSUpdate(dir)
	default:
		usage()
		os.Exit(1)
	}
}

// ── Commands ──────────────────────────────────────────────────────────────

func cmdInit() {
	if err := os.MkdirAll(ciDir, 0755); err != nil {
		fatal(fmt.Errorf("create %s: %w", ciDir, err))
	}

	// Write default config if not already present.
	if _, err := os.Stat(config.Path); os.IsNotExist(err) {
		if err := config.Default().Save(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not write config: %v\n", err)
		} else {
			fmt.Println("Created .codeindex/config.json")
		}
	}

	addToGitignore(ciDir + "/")
	writeClaudeSettings()
	installHook()

	fmt.Println()
	fmt.Println("Initialized. Next:")
	fmt.Println("  codeindex index .   — build the index")
	fmt.Println("  Then open Claude Code — the MCP server starts automatically.")
}

func cmdMCP() {
	cfg, err := config.Load()
	fatal(err)

	bolt, err := store.NewBoltStore(dbPath())
	fatal(err)
	defer bolt.Close()

	fts, err := store.NewFTSStore(ftsPath())
	fatal(err)
	defer fts.Close()

	srv := mcp.New(bolt, fts, cfg)
	if err := srv.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp error:", err)
		os.Exit(1)
	}
}

func cmdIndex(dir string) {
	fmt.Printf("Indexing %s ...\n", dir)
	idx, err := indexer.IndexDir(dir)
	fatal(err)

	s, err := store.NewBoltStore(dbPath())
	fatal(err)
	defer s.Close()
	fatal(s.Save(idx))
	fmt.Printf("symbols: %d | refs: %d | files: %d\n", len(idx.Symbols), len(idx.References), idx.FileCount)

	fts, err := store.NewFTSStore(ftsPath())
	fatal(err)
	defer fts.Close()
	indexed, unchanged, removed, err := fts.IndexDir(dir, false)
	fatal(err)
	fmt.Printf("fts: %d indexed, %d unchanged, %d removed\n", indexed, unchanged, removed)
}

func cmdFTS(query string) {
	fts, err := store.NewFTSStore(ftsPath())
	fatal(err)
	defer fts.Close()

	results, err := fts.Search(query, 50)
	fatal(err)
	if len(results) == 0 {
		fmt.Printf("No results for: %s\n", query)
		return
	}
	fmt.Printf("Found %d result(s) for %q:\n\n", len(results), query)
	for _, r := range results {
		fmt.Printf("%s:%d\n  %s\n\n", r.File, r.Line, strings.TrimSpace(r.Snippet))
	}
}

func cmdFTSUpdate(dir string) {
	fts, err := store.NewFTSStore(ftsPath())
	fatal(err)
	defer fts.Close()

	indexed, unchanged, removed, err := fts.IndexDir(dir, false)
	fatal(err)
	fmt.Printf("fts-update: %d indexed, %d unchanged, %d removed\n", indexed, unchanged, removed)
}

func cmdFind(name string) {
	s := openStore()
	defer s.Close()
	syms, err := s.FindSymbol(name)
	fatal(err)
	if len(syms) == 0 {
		fmt.Printf("No symbol found: %s\n", name)
		return
	}
	for _, sym := range syms {
		printSymbol(sym)
	}
}

func cmdUsages(name string) {
	s := openStore()
	defer s.Close()
	refs, err := s.FindRefs(name)
	fatal(err)
	if len(refs) == 0 {
		fmt.Printf("No usages found: %s\n", name)
		return
	}
	fmt.Printf("Usages of '%s' (%d):\n", name, len(refs))
	for _, r := range refs {
		in := ""
		if r.InSymbol != "" {
			in = "  ← " + r.InSymbol
		}
		fmt.Printf("  %s:%d%s\n", r.File, r.Line, in)
	}
}

func cmdSearch(pattern, kind string) {
	s := openStore()
	defer s.Close()
	syms, err := s.Search(pattern, kind)
	fatal(err)
	if len(syms) == 0 {
		fmt.Printf("No symbols matching: %s\n", pattern)
		return
	}
	fmt.Printf("Found %d symbol(s) matching '%s':\n", len(syms), pattern)
	for _, sym := range syms {
		printSymbol(sym)
	}
}

func cmdList(kind string) {
	s := openStore()
	defer s.Close()
	syms, err := s.ListSymbols(kind)
	fatal(err)
	if len(syms) == 0 {
		fmt.Printf("No symbols (kind=%q)\n", kind)
		return
	}
	for _, sym := range syms {
		recv := ""
		if sym.Receiver != "" {
			recv = sym.Receiver + "."
		}
		fmt.Printf("[%-9s] %s%s\t%s:%d\n", sym.Kind, recv, sym.Name, sym.File, sym.Line)
	}
}

func cmdCallsFrom(inSym string) {
	s := openStore()
	defer s.Close()
	refs, err := s.CallsFrom(inSym)
	fatal(err)
	if len(refs) == 0 {
		fmt.Printf("No calls found from: %s\n", inSym)
		return
	}
	fmt.Printf("Calls inside '%s' (%d):\n", inSym, len(refs))
	for _, r := range refs {
		fmt.Printf("  %s()  at %s:%d\n", r.Symbol, r.File, r.Line)
	}
}

func cmdStats() {
	s := openStore()
	defer s.Close()
	meta, err := s.Stats()
	fatal(err)
	fmt.Printf("files: %d | indexed: %s\n", meta.FileCount, meta.IndexedAt.Format("2006-01-02 15:04:05"))
}

// ── Init helpers ──────────────────────────────────────────────────────────

func addToGitignore(entry string) {
	const path = ".gitignore"
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == entry {
				f.Close()
				return
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not update .gitignore: %v\n", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, entry)
	fmt.Println("Added", entry, "to .gitignore")
}

func writeClaudeSettings() {
	const path = ".claude/settings.json"

	var settings map[string]any
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &settings) //nolint
	}
	if settings == nil {
		settings = map[string]any{}
	}

	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers["codeindex"] = map[string]any{
		"command": "codeindex",
		"args":    []string{"mcp"},
		"type":    "stdio",
	}
	settings["mcpServers"] = mcpServers

	os.MkdirAll(".claude", 0755) //nolint
	data, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not write .claude/settings.json: %v\n", err)
		return
	}
	fmt.Println("Updated .claude/settings.json — MCP server registered")
}

func installHook() {
	const hookPath = ".git/hooks/pre-commit"
	const hookBody = "#!/bin/sh\ncodeindex index .\n"

	if _, err := os.Stat(".git"); os.IsNotExist(err) {
		fmt.Println("No .git directory — skipping pre-commit hook")
		return
	}
	if err := os.MkdirAll(".git/hooks", 0755); err != nil {
		return
	}
	if _, err := os.Stat(hookPath); err == nil {
		fmt.Println("pre-commit hook already exists — skipping")
		return
	}
	if err := os.WriteFile(hookPath, []byte(hookBody), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "warn: could not install pre-commit hook: %v\n", err)
		return
	}
	fmt.Println("Installed pre-commit hook →", hookPath)
}

// ── Helpers ───────────────────────────────────────────────────────────────

func openStore() *store.BoltStore {
	s, err := store.NewBoltStore(dbPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: no index found — run: codeindex index [dir]")
		os.Exit(1)
	}
	return s
}

func printSymbol(s indexer.Symbol) {
	recv := ""
	if s.Receiver != "" {
		recv = "(" + s.Receiver + ") "
	}
	fmt.Printf("[%s] %s%s\n  file: %s:%d\n  pkg:  %s\n", s.Kind, recv, s.Name, s.File, s.Line, s.Package)
	if s.Signature != "" {
		fmt.Printf("  sig:  %s\n", s.Signature)
	}
	fmt.Println()
}

func needArg(usage string) {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "usage: codeindex %s\n", usage)
		os.Exit(1)
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(strings.TrimSpace(`
codeindex — Go codebase indexer (tree-sitter + BoltDB + FTS5)

Commands:
  init                     Initialize codeindex in current project (run once)
  index [dir]              Build the index (default: current dir)
  find <name>              Find where a symbol is defined
  usages <name>            Find all call sites / usages of a symbol
  search <pattern> [kind]  Search symbols (kind: func|method|struct|interface|type|var|const)
  list [kind]              List all symbols by kind
  calls-from <Sym.Method>  Show all calls made inside a function/method
  fts <phrase>             Full-text search source code
  fts-update [dir]         Incrementally update FTS index
  stats                    Show index metadata
  mcp                      Start MCP server (used by Claude Code — do not run manually)

Run "codeindex init" first in any new project.`))
}
