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
		for _, name := range [][]byte{bMeta, bSymName, bSymKind, bRefSym, bRefCaller} {
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
		for _, name := range [][]byte{bMeta, bSymName, bSymKind, bRefSym, bRefCaller} {
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
