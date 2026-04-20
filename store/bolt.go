package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ddbera95/codeindex/indexer"

	bolt "go.etcd.io/bbolt"
)

// BoltStore uses BoltDB — a pure-Go embedded B+ tree key-value store.
//
// Schema (all keys/values are []byte):
//
//	meta           bucket  → file_count (uint64), indexed_at (string)
//	sym_by_name    bucket  → name         → JSON([]Symbol)
//	sym_by_kind    bucket  → "kind\x00name" → name          (secondary index for ListSymbols)
//	ref_by_sym     bucket  → sym_name     → JSON([]Reference)
//	ref_by_caller  bucket  → caller_name  → JSON([]Reference)
var (
	bMeta      = []byte("meta")
	bSymName   = []byte("sym_by_name")
	bSymKind   = []byte("sym_by_kind")
	bRefSym    = []byte("ref_by_sym")
	bRefCaller = []byte("ref_by_caller")

	// Incremental tracking: per-file contribution lists.
	bFileMeta    = []byte("file_meta")     // path → mtime uint64
	bFileSym     = []byte("file_syms")     // path → JSON([]string symNames)
	bFileRefSyms = []byte("file_ref_syms") // path → JSON([]string refTargetSyms)
	bFileCallers = []byte("file_callers")  // path → JSON([]string callerNames)

	kFileCount = []byte("file_count")
	kIndexedAt = []byte("indexed_at")
)

type Meta struct {
	FileCount int
	IndexedAt time.Time
}

type BoltStore struct {
	db   *bolt.DB
	path string
}

func NewBoltStore(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}
	// Pre-create buckets once.
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bMeta, bSymName, bSymKind, bRefSym, bRefCaller, bFileMeta, bFileSym, bFileRefSyms, bFileCallers} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &BoltStore{db: db, path: path}, nil
}

func (s *BoltStore) Name() string { return "BoltDB" }
func (s *BoltStore) Path() string { return s.path }
func (s *BoltStore) Close() error { return s.db.Close() }

// ── Write ─────────────────────────────────────────────────────────────────

func (s *BoltStore) Save(idx *indexer.Index) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		// Clear all buckets cleanly.
		for _, name := range [][]byte{bMeta, bSymName, bSymKind, bRefSym, bRefCaller, bFileMeta, bFileSym, bFileRefSyms, bFileCallers} {
			tx.DeleteBucket(name)
			tx.CreateBucket(name)
		}

		// ── meta ──────────────────────────────────────────────
		meta := tx.Bucket(bMeta)
		meta.Put(kFileCount, uint64ToBytes(uint64(idx.FileCount)))
		meta.Put(kIndexedAt, []byte(idx.IndexedAt.Format(time.RFC3339)))

		// ── symbols ───────────────────────────────────────────
		symName := tx.Bucket(bSymName)
		symKind := tx.Bucket(bSymKind)

		// Group symbols by name so one key holds all matches.
		grouped := map[string][]indexer.Symbol{}
		for _, sym := range idx.Symbols {
			grouped[sym.Name] = append(grouped[sym.Name], sym)
		}
		for name, syms := range grouped {
			data, err := json.Marshal(syms)
			if err != nil {
				return err
			}
			if err := symName.Put([]byte(name), data); err != nil {
				return err
			}
			// Secondary index key: "kind\x00name" so we can cursor-seek by kind prefix.
			for _, sym := range syms {
				kindKey := sym.Kind + "\x00" + name
				symKind.Put([]byte(kindKey), []byte(name))
			}
		}

		// ── references ────────────────────────────────────────
		refSym := tx.Bucket(bRefSym)
		refCaller := tx.Bucket(bRefCaller)

		bySymbol := map[string][]indexer.Reference{}
		byCaller := map[string][]indexer.Reference{}
		for _, r := range idx.References {
			bySymbol[r.Symbol] = append(bySymbol[r.Symbol], r)
			if r.InSymbol != "" {
				byCaller[r.InSymbol] = append(byCaller[r.InSymbol], r)
			}
		}
		for sym, refs := range bySymbol {
			data, _ := json.Marshal(refs)
			refSym.Put([]byte(sym), data)
		}
		for caller, refs := range byCaller {
			data, _ := json.Marshal(refs)
			refCaller.Put([]byte(caller), data)
		}

		// ── incremental tracking ───────────────────────────────
		fileSymNamesMap := map[string]map[string]bool{}
		fileRefSymMap := map[string]map[string]bool{}
		fileCallerMap := map[string]map[string]bool{}
		for _, sym := range idx.Symbols {
			if fileSymNamesMap[sym.File] == nil {
				fileSymNamesMap[sym.File] = map[string]bool{}
			}
			fileSymNamesMap[sym.File][sym.Name] = true
		}
		for _, r := range idx.References {
			if fileRefSymMap[r.File] == nil {
				fileRefSymMap[r.File] = map[string]bool{}
			}
			fileRefSymMap[r.File][r.Symbol] = true
			if r.InSymbol != "" {
				if fileCallerMap[r.File] == nil {
					fileCallerMap[r.File] = map[string]bool{}
				}
				fileCallerMap[r.File][r.InSymbol] = true
			}
		}
		fSymB := tx.Bucket(bFileSym)
		fRefB := tx.Bucket(bFileRefSyms)
		fCalB := tx.Bucket(bFileCallers)
		fMetB := tx.Bucket(bFileMeta)
		for path, nameSet := range fileSymNamesMap {
			names := make([]string, 0, len(nameSet))
			for n := range nameSet {
				names = append(names, n)
			}
			data, _ := json.Marshal(names)
			fSymB.Put([]byte(path), data)
			fMetB.Put([]byte(path), uint64ToBytes(0))
		}
		for path, symSet := range fileRefSymMap {
			syms := make([]string, 0, len(symSet))
			for s := range symSet {
				syms = append(syms, s)
			}
			data, _ := json.Marshal(syms)
			fRefB.Put([]byte(path), data)
		}
		for path, callerSet := range fileCallerMap {
			callers := make([]string, 0, len(callerSet))
			for c := range callerSet {
				callers = append(callers, c)
			}
			data, _ := json.Marshal(callers)
			fCalB.Put([]byte(path), data)
		}

		return nil
	})
}

// ── Queries ────────────────────────────────────────────────────────────────

// FindSymbol returns all symbols whose name exactly matches.
func (s *BoltStore) FindSymbol(name string) ([]indexer.Symbol, error) {
	return s.viewSymbol(bSymName, []byte(name))
}

// FindRefs returns all references (call sites, var/const usages) for the symbol.
func (s *BoltStore) FindRefs(sym string) ([]indexer.Reference, error) {
	return s.viewRefs(bRefSym, []byte(sym))
}

// CallsFrom returns all calls made inside a function/method (e.g. "PostService.PublishPost").
func (s *BoltStore) CallsFrom(inSym string) ([]indexer.Reference, error) {
	return s.viewRefs(bRefCaller, []byte(inSym))
}

// ListSymbols returns all symbols, optionally filtered by kind.
func (s *BoltStore) ListSymbols(kind string) ([]indexer.Symbol, error) {
	var out []indexer.Symbol
	err := s.db.View(func(tx *bolt.Tx) error {
		symName := tx.Bucket(bSymName)
		symKind := tx.Bucket(bSymKind)

		if kind == "" || kind == "all" {
			// Full scan of sym_by_name.
			return symName.ForEach(func(_, v []byte) error {
				var syms []indexer.Symbol
				if err := json.Unmarshal(v, &syms); err != nil {
					return err
				}
				out = append(out, syms...)
				return nil
			})
		}

		// Seek to "kind\x00" prefix in sym_by_kind.
		prefix := []byte(kind + "\x00")
		c := symKind.Cursor()
		seen := map[string]bool{}
		for k, nameBytes := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, nameBytes = c.Next() {
			name := string(nameBytes)
			if seen[name] {
				continue
			}
			seen[name] = true
			v := symName.Get(nameBytes)
			if v == nil {
				continue
			}
			var syms []indexer.Symbol
			if err := json.Unmarshal(v, &syms); err != nil {
				return err
			}
			for _, sym := range syms {
				if sym.Kind == kind {
					out = append(out, sym)
				}
			}
		}
		return nil
	})
	return out, err
}

// Search returns symbols whose name contains pattern (case-insensitive), optionally filtered by kind.
func (s *BoltStore) Search(pattern, kind string) ([]indexer.Symbol, error) {
	lower := strings.ToLower(pattern)
	var out []indexer.Symbol
	err := s.db.View(func(tx *bolt.Tx) error {
		symName := tx.Bucket(bSymName)
		return symName.ForEach(func(k, v []byte) error {
			if !strings.Contains(strings.ToLower(string(k)), lower) {
				return nil
			}
			var syms []indexer.Symbol
			if err := json.Unmarshal(v, &syms); err != nil {
				return err
			}
			for _, sym := range syms {
				if kind == "" || kind == "all" || sym.Kind == kind {
					out = append(out, sym)
				}
			}
			return nil
		})
	})
	return out, err
}

// Stats returns index metadata.
func (s *BoltStore) Stats() (Meta, error) {
	var m Meta
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bMeta)
		if b == nil {
			return nil
		}
		if v := b.Get(kFileCount); v != nil {
			m.FileCount = int(binary.BigEndian.Uint64(v))
		}
		if v := b.Get(kIndexedAt); v != nil {
			m.IndexedAt, _ = time.Parse(time.RFC3339, string(v))
		}
		return nil
	})
	return m, err
}

// ── Incremental file updates ───────────────────────────────────────────────

// GetFileSymNames returns the symbol names that path currently contributes to the index.
func (s *BoltStore) GetFileSymNames(path string) ([]string, error) {
	var names []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bFileSym)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(path))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &names)
	})
	return names, err
}

// FilesReferencingSymbols returns the unique file paths that reference any of the given symbol names.
// Used to find which files need re-indexing after a symbol is renamed or removed.
func (s *BoltStore) FilesReferencingSymbols(symNames []string) ([]string, error) {
	seen := map[string]bool{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bRefSym)
		if b == nil {
			return nil
		}
		for _, name := range symNames {
			v := b.Get([]byte(name))
			if v == nil {
				continue
			}
			var refs []indexer.Reference
			if err := json.Unmarshal(v, &refs); err != nil {
				continue
			}
			for _, r := range refs {
				seen[r.File] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	return out, err
}

// GetKnownVC returns all var/const names from the index for incremental parsing.
func (s *BoltStore) GetKnownVC() (map[string]string, error) {
	out := map[string]string{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bSymName)
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var syms []indexer.Symbol
			if err := json.Unmarshal(v, &syms); err != nil {
				return nil
			}
			for _, sym := range syms {
				if sym.Kind == "var" || sym.Kind == "const" {
					out[sym.Name] = sym.Kind
				}
			}
			return nil
		})
	})
	return out, err
}

// RemoveFile surgically removes all symbols and refs contributed by path.
func (s *BoltStore) RemoveFile(path string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return removeFileTx(tx, path)
	})
}

// UpdateFile atomically replaces all data for path with fresh syms and refs.
func (s *BoltStore) UpdateFile(path string, mtime int64, syms []indexer.Symbol, refs []indexer.Reference) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := removeFileTx(tx, path); err != nil {
			return err
		}
		return insertFileTx(tx, path, mtime, syms, refs)
	})
}

func removeFileTx(tx *bolt.Tx, path string) error {
	symName := tx.Bucket(bSymName)
	symKind := tx.Bucket(bSymKind)
	refSym := tx.Bucket(bRefSym)
	refCaller := tx.Bucket(bRefCaller)
	fileSym := tx.Bucket(bFileSym)
	fileRefSyms := tx.Bucket(bFileRefSyms)
	fileCallers := tx.Bucket(bFileCallers)
	fileMeta := tx.Bucket(bFileMeta)

	if symName == nil || fileSym == nil {
		return nil
	}
	pathKey := []byte(path)

	// Remove this file's symbol contributions.
	if v := fileSym.Get(pathKey); v != nil {
		var names []string
		json.Unmarshal(v, &names) //nolint
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] {
				continue
			}
			seen[name] = true
			nameKey := []byte(name)
			v2 := symName.Get(nameKey)
			if v2 == nil {
				continue
			}
			var syms []indexer.Symbol
			json.Unmarshal(v2, &syms) //nolint

			oldKinds := map[string]bool{}
			for _, sym := range syms {
				oldKinds[sym.Kind] = true
			}
			var filtered []indexer.Symbol
			for _, sym := range syms {
				if sym.File != path {
					filtered = append(filtered, sym)
				}
			}
			newKinds := map[string]bool{}
			for _, sym := range filtered {
				newKinds[sym.Kind] = true
			}
			for k := range oldKinds {
				symKind.Delete([]byte(k + "\x00" + name)) //nolint
			}
			for k := range newKinds {
				symKind.Put([]byte(k+"\x00"+name), nameKey) //nolint
			}
			if len(filtered) == 0 {
				symName.Delete(nameKey) //nolint
			} else {
				data, _ := json.Marshal(filtered)
				symName.Put(nameKey, data) //nolint
			}
		}
		fileSym.Delete(pathKey) //nolint
	}

	// Remove from ref_by_sym.
	if fileRefSyms != nil {
		if v := fileRefSyms.Get(pathKey); v != nil {
			var names []string
			json.Unmarshal(v, &names) //nolint
			seen := map[string]bool{}
			for _, sym := range names {
				if seen[sym] {
					continue
				}
				seen[sym] = true
				symKey := []byte(sym)
				v2 := refSym.Get(symKey)
				if v2 == nil {
					continue
				}
				var refs []indexer.Reference
				json.Unmarshal(v2, &refs) //nolint
				var filtered []indexer.Reference
				for _, r := range refs {
					if r.File != path {
						filtered = append(filtered, r)
					}
				}
				if len(filtered) == 0 {
					refSym.Delete(symKey) //nolint
				} else {
					data, _ := json.Marshal(filtered)
					refSym.Put(symKey, data) //nolint
				}
			}
			fileRefSyms.Delete(pathKey) //nolint
		}
	}

	// Remove from ref_by_caller.
	if fileCallers != nil {
		if v := fileCallers.Get(pathKey); v != nil {
			var names []string
			json.Unmarshal(v, &names) //nolint
			seen := map[string]bool{}
			for _, caller := range names {
				if seen[caller] {
					continue
				}
				seen[caller] = true
				callerKey := []byte(caller)
				v2 := refCaller.Get(callerKey)
				if v2 == nil {
					continue
				}
				var refs []indexer.Reference
				json.Unmarshal(v2, &refs) //nolint
				var filtered []indexer.Reference
				for _, r := range refs {
					if r.File != path {
						filtered = append(filtered, r)
					}
				}
				if len(filtered) == 0 {
					refCaller.Delete(callerKey) //nolint
				} else {
					data, _ := json.Marshal(filtered)
					refCaller.Put(callerKey, data) //nolint
				}
			}
			fileCallers.Delete(pathKey) //nolint
		}
	}

	if fileMeta != nil {
		fileMeta.Delete(pathKey) //nolint
	}
	return nil
}

func insertFileTx(tx *bolt.Tx, path string, mtime int64, syms []indexer.Symbol, refs []indexer.Reference) error {
	symName := tx.Bucket(bSymName)
	symKind := tx.Bucket(bSymKind)
	refSym := tx.Bucket(bRefSym)
	refCaller := tx.Bucket(bRefCaller)
	fileSym := tx.Bucket(bFileSym)
	fileRefSyms := tx.Bucket(bFileRefSyms)
	fileCallers := tx.Bucket(bFileCallers)
	fileMeta := tx.Bucket(bFileMeta)
	pathKey := []byte(path)

	// Insert symbols, merging into existing entries (other files may share the name).
	symNameSet := map[string]bool{}
	for _, sym := range syms {
		name := sym.Name
		symNameSet[name] = true
		nameKey := []byte(name)

		var existing []indexer.Symbol
		if v := symName.Get(nameKey); v != nil {
			json.Unmarshal(v, &existing) //nolint
		}
		existing = append(existing, sym)
		data, _ := json.Marshal(existing)
		symName.Put(nameKey, data)           //nolint
		symKind.Put([]byte(sym.Kind+"\x00"+name), nameKey) //nolint
	}
	symNames := make([]string, 0, len(symNameSet))
	for n := range symNameSet {
		symNames = append(symNames, n)
	}
	if data, _ := json.Marshal(symNames); data != nil {
		fileSym.Put(pathKey, data) //nolint
	}

	// Insert refs, merging into existing entries.
	bySymbol := map[string][]indexer.Reference{}
	byCaller := map[string][]indexer.Reference{}
	for _, r := range refs {
		bySymbol[r.Symbol] = append(bySymbol[r.Symbol], r)
		if r.InSymbol != "" {
			byCaller[r.InSymbol] = append(byCaller[r.InSymbol], r)
		}
	}

	refSymNames := make([]string, 0, len(bySymbol))
	for sym, newRefs := range bySymbol {
		refSymNames = append(refSymNames, sym)
		symKey := []byte(sym)
		var existing []indexer.Reference
		if v := refSym.Get(symKey); v != nil {
			json.Unmarshal(v, &existing) //nolint
		}
		existing = append(existing, newRefs...)
		data, _ := json.Marshal(existing)
		refSym.Put(symKey, data) //nolint
	}
	if data, _ := json.Marshal(refSymNames); data != nil {
		fileRefSyms.Put(pathKey, data) //nolint
	}

	callerNames := make([]string, 0, len(byCaller))
	for caller, newRefs := range byCaller {
		callerNames = append(callerNames, caller)
		callerKey := []byte(caller)
		var existing []indexer.Reference
		if v := refCaller.Get(callerKey); v != nil {
			json.Unmarshal(v, &existing) //nolint
		}
		existing = append(existing, newRefs...)
		data, _ := json.Marshal(existing)
		refCaller.Put(callerKey, data) //nolint
	}
	if data, _ := json.Marshal(callerNames); data != nil {
		fileCallers.Put(pathKey, data) //nolint
	}

	fileMeta.Put(pathKey, uint64ToBytes(uint64(mtime))) //nolint
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func (s *BoltStore) viewSymbol(bucket, key []byte) ([]indexer.Symbol, error) {
	var out []indexer.Symbol
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func (s *BoltStore) viewRefs(bucket, key []byte) ([]indexer.Reference, error) {
	var out []indexer.Reference
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		v := b.Get(key)
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func uint64ToBytes(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}
