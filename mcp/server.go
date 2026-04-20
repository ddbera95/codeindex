package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ddbera95/codeindex/config"
	"github.com/ddbera95/codeindex/indexer"
	"github.com/ddbera95/codeindex/store"

	"github.com/fsnotify/fsnotify"
)

// JSON-RPC 2.0 types
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

type property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server is the MCP stdio server for codeindex.
type Server struct {
	bolt          *store.BoltStore
	fts           *store.FTSStore
	cfg           *config.Config
	boltPath      string
	mu            sync.Mutex
	enc           *json.Encoder
	reloadPending atomic.Bool // set by watcher when index.bolt.new appears
}

func New(bolt *store.BoltStore, fts *store.FTSStore, cfg *config.Config, boltPath string) *Server {
	return &Server{bolt: bolt, fts: fts, cfg: cfg, boltPath: boltPath, enc: json.NewEncoder(os.Stdout)}
}

func (s *Server) Run() error {
	go s.watchFiles(".")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Fprintln(os.Stderr, "codeindex mcp parse error:", err)
			continue
		}
		s.dispatch(req)
	}
	return scanner.Err()
}

func (s *Server) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enc.Encode(v) //nolint
}

func (s *Server) reply(id any, result any) {
	s.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) replyErr(id any, code int, msg string) {
	s.send(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) text(t string) map[string]any {
	return map[string]any{"content": []mcpContent{{Type: "text", Text: t}}}
}

func (s *Server) dispatch(req request) {
	// Hot-reload: pick up a full rebuild written by `codeindex index` while we were running.
	if s.reloadPending.Load() {
		s.reloadPending.Store(false)
		staging := s.boltPath + ".new"
		if err := s.bolt.Close(); err == nil {
			if err := os.Rename(staging, s.boltPath); err == nil {
				if b, err := store.NewBoltStore(s.boltPath); err == nil {
					s.bolt = b
					fmt.Fprintln(os.Stderr, "codeindex sym: hot-reloaded from full rebuild")
				}
			}
		}
	}

	switch req.Method {
	case "initialize":
		s.reply(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "codeindex", "version": "1.0.0"},
			"instructions": "This project uses codeindex for Go code navigation. " +
				"ALWAYS prefer codeindex tools over reading files or grep. Rules:\n" +
				"- find_symbol: locate any Go definition — never open files just to find where something is defined.\n" +
				"- find_usages: trace all call sites — never grep for function names.\n" +
				"- search_symbols: discover types/functions by name pattern.\n" +
				"- calls_from: understand what a function depends on.\n" +
				"- full_text_search: search comments, error strings, TODOs, string literals.\n" +
				"- get_stats: check index freshness before starting a task.\n" +
				"- rebuild_index: full rebuild — only needed after very large refactors.\n" +
				"- update_fts: call after adding a new FTS pattern via update_config.\n" +
				"Both symbol index and FTS update automatically on every file save (incremental). " +
				"Only call rebuild_index after large refactors or if data looks stale. " +
				"Only read source files when you need the full implementation body.",
		})
	case "notifications/initialized", "notifications/cancelled":
		// no response for notifications
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolList()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.replyErr(req.ID, -32600, "invalid params")
			return
		}
		s.callTool(req.ID, p.Name, p.Arguments)
	default:
		s.replyErr(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) callTool(id any, name string, args json.RawMessage) {
	var a map[string]any
	json.Unmarshal(args, &a) //nolint
	str := func(k string) string { v, _ := a[k].(string); return v }
	num := func(k string, def int) int {
		if v, ok := a[k].(float64); ok {
			return int(v)
		}
		return def
	}

	switch name {
	case "find_symbol":
		s.toolFindSymbol(id, str("name"))
	case "find_usages":
		s.toolFindUsages(id, str("name"))
	case "search_symbols":
		s.toolSearchSymbols(id, str("pattern"), str("kind"))
	case "full_text_search":
		s.toolFTS(id, str("query"), num("limit", 20))
	case "calls_from":
		s.toolCallsFrom(id, str("symbol"))
	case "get_config":
		s.toolGetConfig(id)
	case "update_config":
		s.toolUpdateConfig(id, str("addFTSPattern"), str("removeFTSPattern"), num("maxFileSizeKB", 0))
	case "get_stats":
		s.toolGetStats(id)
	case "rebuild_index":
		s.toolRebuildIndex(id)
	case "update_fts":
		s.toolUpdateFTS(id)
	default:
		s.replyErr(id, -32601, "unknown tool: "+name)
	}
}

// ── Tool handlers ─────────────────────────────────────────────────────────

func (s *Server) toolFindSymbol(id any, name string) {
	if name == "" {
		s.reply(id, s.text("error: name is required"))
		return
	}
	syms, err := s.bolt.FindSymbol(name)
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	if len(syms) == 0 {
		s.reply(id, s.text("No symbol found: "+name))
		return
	}
	var sb strings.Builder
	for _, sym := range syms {
		recv := ""
		if sym.Receiver != "" {
			recv = "(" + sym.Receiver + ") "
		}
		fmt.Fprintf(&sb, "[%s] %s%s\n  file: %s:%d\n  pkg:  %s\n", sym.Kind, recv, sym.Name, sym.File, sym.Line, sym.Package)
		if sym.Signature != "" {
			fmt.Fprintf(&sb, "  sig:  %s\n", sym.Signature)
		}
		sb.WriteByte('\n')
	}
	s.reply(id, s.text(sb.String()))
}

func (s *Server) toolFindUsages(id any, name string) {
	if name == "" {
		s.reply(id, s.text("error: name is required"))
		return
	}
	refs, err := s.bolt.FindRefs(name)
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	if len(refs) == 0 {
		s.reply(id, s.text("No usages found: "+name))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Usages of '%s' (%d):\n", name, len(refs))
	for _, r := range refs {
		in := ""
		if r.InSymbol != "" {
			in = "  ← " + r.InSymbol
		}
		fmt.Fprintf(&sb, "  %s:%d%s\n", r.File, r.Line, in)
	}
	s.reply(id, s.text(sb.String()))
}

func (s *Server) toolSearchSymbols(id any, pattern, kind string) {
	if pattern == "" {
		s.reply(id, s.text("error: pattern is required"))
		return
	}
	syms, err := s.bolt.Search(pattern, kind)
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	if len(syms) == 0 {
		s.reply(id, s.text("No symbols matching: "+pattern))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d symbol(s) matching '%s':\n\n", len(syms), pattern)
	for _, sym := range syms {
		recv := ""
		if sym.Receiver != "" {
			recv = sym.Receiver + "."
		}
		fmt.Fprintf(&sb, "[%-9s] %s%s  %s:%d\n", sym.Kind, recv, sym.Name, sym.File, sym.Line)
		if sym.Signature != "" {
			fmt.Fprintf(&sb, "             sig: %s\n", sym.Signature)
		}
	}
	s.reply(id, s.text(sb.String()))
}

func (s *Server) toolFTS(id any, query string, limit int) {
	if query == "" {
		s.reply(id, s.text("error: query is required"))
		return
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	results, err := s.fts.Search(query, limit)
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	if len(results) == 0 {
		s.reply(id, s.text("No results for: "+query))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d result(s) for %q:\n\n", len(results), query)
	for _, r := range results {
		fmt.Fprintf(&sb, "%s:%d\n  %s\n\n", r.File, r.Line, strings.TrimSpace(r.Snippet))
	}
	s.reply(id, s.text(sb.String()))
}

func (s *Server) toolCallsFrom(id any, symbol string) {
	if symbol == "" {
		s.reply(id, s.text("error: symbol is required"))
		return
	}
	refs, err := s.bolt.CallsFrom(symbol)
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	if len(refs) == 0 {
		s.reply(id, s.text("No calls found from: "+symbol))
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Calls inside '%s' (%d):\n", symbol, len(refs))
	for _, r := range refs {
		fmt.Fprintf(&sb, "  %s()  at %s:%d\n", r.Symbol, r.File, r.Line)
	}
	s.reply(id, s.text(sb.String()))
}

func (s *Server) toolGetConfig(id any) {
	data, _ := json.MarshalIndent(s.cfg, "", "  ")
	s.reply(id, s.text(string(data)))
}

func (s *Server) toolUpdateConfig(id any, addPattern, removePattern string, maxSizeKB int) {
	var msgs []string

	if addPattern != "" {
		if addPattern == "*" || addPattern == "*.*" {
			msgs = append(msgs, "ERROR: Pattern '"+addPattern+"' matches ALL files including binaries. Refusing to add.")
		} else {
			large := s.scanLargeFiles(".", addPattern)
			if len(large) > 0 {
				msgs = append(msgs, fmt.Sprintf("WARNING: %d file(s) matching '%s' exceed %dKB and will be skipped:", len(large), addPattern, s.cfg.FTS.MaxFileSizeKB))
				for _, f := range large {
					msgs = append(msgs, "  "+f)
				}
			}
			found := false
			for _, p := range s.cfg.FTS.Include {
				if p == addPattern {
					found = true
					break
				}
			}
			if found {
				msgs = append(msgs, "Pattern already exists: "+addPattern)
			} else {
				s.cfg.FTS.Include = append(s.cfg.FTS.Include, addPattern)
				msgs = append(msgs, "Added FTS pattern: "+addPattern+". Run 'codeindex index .' to index existing files.")
			}
		}
	}

	if removePattern != "" {
		filtered := s.cfg.FTS.Include[:0]
		removed := false
		for _, p := range s.cfg.FTS.Include {
			if p == removePattern {
				removed = true
			} else {
				filtered = append(filtered, p)
			}
		}
		s.cfg.FTS.Include = filtered
		if removed {
			msgs = append(msgs, "Removed FTS pattern: "+removePattern)
		} else {
			msgs = append(msgs, "Pattern not found: "+removePattern)
		}
	}

	if maxSizeKB > 0 {
		if maxSizeKB > 500 {
			msgs = append(msgs, fmt.Sprintf("WARNING: %dKB is very large. Files over 500KB are usually generated/binary. Recommended max: 100KB.", maxSizeKB))
		}
		s.cfg.FTS.MaxFileSizeKB = maxSizeKB
		msgs = append(msgs, fmt.Sprintf("Set maxFileSizeKB to %d", maxSizeKB))
	}

	if err := s.cfg.Save(); err != nil {
		msgs = append(msgs, "error saving config: "+err.Error())
	} else {
		msgs = append(msgs, "Config saved to .codeindex/config.json")
	}

	s.reply(id, s.text(strings.Join(msgs, "\n")))
}

func (s *Server) toolRebuildIndex(id any) {
	fmt.Fprintln(os.Stderr, "codeindex mcp: rebuilding symbol index...")
	idx, err := indexer.IndexDir(".")
	if err != nil {
		s.reply(id, s.text("error rebuilding index: "+err.Error()))
		return
	}
	if err := s.bolt.Save(idx); err != nil {
		s.reply(id, s.text("error saving index: "+err.Error()))
		return
	}
	s.reply(id, s.text(fmt.Sprintf(
		"Symbol index rebuilt: %d files | %d symbols | %d refs",
		idx.FileCount, len(idx.Symbols), len(idx.References),
	)))
}

func (s *Server) toolUpdateFTS(id any) {
	fmt.Fprintln(os.Stderr, "codeindex mcp: updating FTS index...")
	indexed, unchanged, removed, err := s.fts.IndexDir(".", false)
	if err != nil {
		s.reply(id, s.text("error updating FTS: "+err.Error()))
		return
	}
	s.reply(id, s.text(fmt.Sprintf(
		"FTS updated: %d indexed, %d unchanged, %d removed",
		indexed, unchanged, removed,
	)))
}

func (s *Server) scanLargeFiles(dir, pattern string) []string {
	maxBytes := int64(s.cfg.FTS.MaxFileSizeKB) * 1024
	var large []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint
		if err != nil || d.IsDir() {
			return nil
		}
		if s.cfg.IsExcludedDir(filepath.Base(filepath.Dir(path))) {
			return filepath.SkipDir
		}
		if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
			if info, err := d.Info(); err == nil && info.Size() > maxBytes {
				large = append(large, fmt.Sprintf("%s (%.1fKB)", path, float64(info.Size())/1024))
			}
		}
		return nil
	})
	return large
}

func (s *Server) toolGetStats(id any) {
	meta, err := s.bolt.Stats()
	if err != nil {
		s.reply(id, s.text("error: "+err.Error()))
		return
	}
	files, lines, _ := s.fts.Stats()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Symbol index:\n  files:   %d\n  indexed: %s\n\n",
		meta.FileCount, meta.IndexedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&sb, "FTS index:\n  files: %d\n  lines: %d\n\n", files, lines)
	fmt.Fprintf(&sb, "Config:\n  fts patterns:  %s\n  max file size: %dKB\n  exclude dirs:  %s\n",
		strings.Join(s.cfg.FTS.Include, ", "),
		s.cfg.FTS.MaxFileSizeKB,
		strings.Join(s.cfg.FTS.ExcludeDirs, ", "),
	)
	s.reply(id, s.text(sb.String()))
}

// ── File watcher ─────────────────────────────────────────────────────────

func (s *Server) incrementalSymbolUpdate(path string) {
	// Find which files reference this file's symbols BEFORE we update,
	// so we can cascade re-index them after (cross-file ref accuracy).
	oldSymNames, _ := s.bolt.GetFileSymNames(path)
	affected, _ := s.bolt.FilesReferencingSymbols(oldSymNames)

	// Update the changed file.
	knownVC, err := s.bolt.GetKnownVC()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codeindex sym: GetKnownVC error:", err)
		return
	}
	syms, refs, err := indexer.IndexFile(path, knownVC)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codeindex sym: parse error", path, err)
		return
	}
	var mtime int64
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime().Unix()
	}
	if err := s.bolt.UpdateFile(path, mtime, syms, refs); err != nil {
		fmt.Fprintln(os.Stderr, "codeindex sym: save error", path, err)
		return
	}
	fmt.Fprintf(os.Stderr, "codeindex sym: updated %s (%d syms, %d refs)\n", path, len(syms), len(refs))

	// Cascade: re-index files that referenced the old symbols so their refs stay accurate.
	for _, dep := range affected {
		if dep == path {
			continue
		}
		knownVC, _ := s.bolt.GetKnownVC()
		dSyms, dRefs, err := indexer.IndexFile(dep, knownVC)
		if err != nil {
			continue // file may have been deleted
		}
		var dMtime int64
		if info, err := os.Stat(dep); err == nil {
			dMtime = info.ModTime().Unix()
		}
		if err := s.bolt.UpdateFile(dep, dMtime, dSyms, dRefs); err != nil {
			fmt.Fprintln(os.Stderr, "codeindex sym: cascade save error", dep, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "codeindex sym: cascade updated %s\n", dep)
	}
}

func (s *Server) watchFiles(dir string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codeindex watcher init error:", err)
		return
	}
	defer watcher.Close()

	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != dir && s.cfg.IsExcludedDir(d.Name()) {
			return filepath.SkipDir
		}
		watcher.Add(path) //nolint
		return nil
	})

	type pending struct{ op fsnotify.Op }
	queue := map[string]pending{}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			isGo := strings.HasSuffix(ev.Name, ".go")
			if isGo || s.cfg.MatchesFTS(ev.Name) {
				queue[ev.Name] = pending{ev.Op}
			}
			// Detect staging file written by `codeindex index` while MCP is running.
			if strings.HasSuffix(ev.Name, ".bolt.new") && ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				s.reloadPending.Store(true)
			}
			// Watch newly created subdirectories.
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					watcher.Add(ev.Name) //nolint
				}
			}
		case <-ticker.C:
			for path, p := range queue {
				delete(queue, path)
				isGo := strings.HasSuffix(path, ".go")
				if p.op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					if isGo {
						// Capture affected files before removal for cascade re-index.
						oldSymNames, _ := s.bolt.GetFileSymNames(path)
						affected, _ := s.bolt.FilesReferencingSymbols(oldSymNames)

						if err := s.bolt.RemoveFile(path); err == nil {
							fmt.Fprintln(os.Stderr, "codeindex sym: removed", path)
						}
						for _, dep := range affected {
							if dep == path {
								continue
							}
							knownVC, _ := s.bolt.GetKnownVC()
							dSyms, dRefs, err := indexer.IndexFile(dep, knownVC)
							if err != nil {
								continue
							}
							var dMtime int64
							if info, err := os.Stat(dep); err == nil {
								dMtime = info.ModTime().Unix()
							}
							s.bolt.UpdateFile(dep, dMtime, dSyms, dRefs) //nolint
							fmt.Fprintln(os.Stderr, "codeindex sym: cascade updated", dep)
						}
					}
					if s.cfg.MatchesFTS(path) {
						if err := s.fts.RemoveFile(path); err == nil {
							fmt.Fprintln(os.Stderr, "codeindex fts: removed", path)
						}
					}
				} else {
					if isGo {
						s.incrementalSymbolUpdate(path)
					}
					if s.cfg.MatchesFTS(path) {
						if err := s.fts.IndexFile(path); err != nil {
							fmt.Fprintln(os.Stderr, "codeindex fts: error", path, err)
						} else {
							fmt.Fprintln(os.Stderr, "codeindex fts: updated", path)
						}
					}
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintln(os.Stderr, "codeindex watcher error:", err)
		}
	}
}

// ── Tool definitions ──────────────────────────────────────────────────────

func toolList() []toolDef {
	return []toolDef{
		{
			Name: "find_symbol",
			Description: "Find where a Go symbol is defined (function, method, struct, interface, variable, constant). " +
				"Returns file path, line number, package, and full signature with parameter types and return types. " +
				"Use this instead of grep to locate definitions.",
			InputSchema: inputSchema{
				Type:       "object",
				Properties: map[string]property{"name": {Type: "string", Description: "Exact symbol name (e.g. 'NewUserService', 'Register', 'UserService')"}},
				Required:   []string{"name"},
			},
		},
		{
			Name: "find_usages",
			Description: "Find all places where a Go symbol is called or referenced across the codebase. " +
				"Returns file:line and the containing function for each usage. Use to understand impact before changing a symbol.",
			InputSchema: inputSchema{
				Type:       "object",
				Properties: map[string]property{"name": {Type: "string", Description: "Symbol name to find usages of"}},
				Required:   []string{"name"},
			},
		},
		{
			Name:        "search_symbols",
			Description: "Search Go symbols by name pattern (case-insensitive substring). Optionally filter by kind. Returns definitions with signatures.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"pattern": {Type: "string", Description: "Name pattern (case-insensitive substring)"},
					"kind":    {Type: "string", Description: "Optional filter: func, method, struct, interface, type, var, const"},
				},
				Required: []string{"pattern"},
			},
		},
		{
			Name: "full_text_search",
			Description: "Full-text search across indexed source files (SQLite FTS5). " +
				"The index updates automatically when files change — no manual refresh needed. " +
				"Query syntax: 'word', '\"exact phrase\"', 'prefix*', 'word1 OR word2'. " +
				"Use for comments, error strings, TODOs, or anything not captured by symbol indexing.",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"query": {Type: "string", Description: "FTS5 query. Examples: 'validateEmail', '\"not found\"', 'auth*', 'TODO OR FIXME'"},
					"limit": {Type: "number", Description: "Max results (default 20, max 50)"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "calls_from",
			Description: "Show all function/method calls made inside a specific function. Use to trace call graphs and understand dependencies.",
			InputSchema: inputSchema{
				Type:       "object",
				Properties: map[string]property{"symbol": {Type: "string", Description: "Use 'ReceiverType.MethodName' for methods (e.g. 'UserService.Register') or 'FunctionName' for functions"}},
				Required:   []string{"symbol"},
			},
		},
		{
			Name:        "get_config",
			Description: "Show current codeindex config: FTS file patterns, excluded dirs, and max file size. The symbol index always covers only *.go files.",
			InputSchema: inputSchema{Type: "object", Properties: map[string]property{}},
		},
		{
			Name: "update_config",
			Description: "Add or remove FTS file patterns, or change the max file size limit. " +
				"The symbol index (find_symbol, find_usages, calls_from) is always Go-only and cannot be changed. " +
				"FTS can be extended to other text file types.\n\n" +
				"WARNINGS — read before updating:\n" +
				"• NEVER add '*' or '*.*' — matches binaries and corrupts the index\n" +
				"• Do NOT add patterns matching files >100KB — they will be skipped but waste scan time\n" +
				"• Safe to add: '*.md', '*.yaml', '*.toml', '*.json', '*.env', '*.ts', '*.py'\n" +
				"• Default maxFileSizeKB is 50 — keep under 100 for config/doc files\n" +
				"• After adding a pattern, run 'codeindex index .' in the terminal to index existing files\n" +
				"• New files matching added patterns will be auto-indexed by the file watcher going forward",
			InputSchema: inputSchema{
				Type: "object",
				Properties: map[string]property{
					"addFTSPattern":    {Type: "string", Description: "Glob pattern to add (e.g. '*.md'). Leave empty if not changing."},
					"removeFTSPattern": {Type: "string", Description: "Glob pattern to remove. Leave empty if not changing."},
					"maxFileSizeKB":    {Type: "number", Description: "Max file size in KB for FTS (files larger are skipped). Pass 0 to leave unchanged."},
				},
			},
		},
		{
			Name:        "get_stats",
			Description: "Index statistics: files, symbols, FTS lines, last index time, and current config.",
			InputSchema: inputSchema{Type: "object", Properties: map[string]property{}},
		},
		{
			Name: "rebuild_index",
			Description: "Rebuild the full symbol index from scratch (find_symbol, find_usages, calls_from, search_symbols). " +
				"The file watcher handles incremental updates automatically on save. " +
				"Use this only after very large refactors or if the index seems stale.",
			InputSchema: inputSchema{Type: "object", Properties: map[string]property{}},
		},
		{
			Name: "update_fts",
			Description: "Incrementally update the full-text search index. " +
				"Only re-indexes files that changed since the last run — fast even on large codebases. " +
				"The file watcher already does this automatically on save, but call this if you want to force a sync " +
				"or after adding a new FTS pattern via update_config.",
			InputSchema: inputSchema{Type: "object", Properties: map[string]property{}},
		},
	}
}
