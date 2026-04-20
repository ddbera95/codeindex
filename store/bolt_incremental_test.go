package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ddbera95/codeindex/indexer"
	"github.com/ddbera95/codeindex/store"
)

// writeFile writes content to a file, creating parent dirs as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func setupIndex(t *testing.T, files map[string]string) (*store.BoltStore, string) {
	t.Helper()
	dir := t.TempDir()
	for rel, src := range files {
		writeFile(t, filepath.Join(dir, rel), src)
	}
	idx, err := indexer.IndexDir(dir)
	if err != nil {
		t.Fatal("IndexDir:", err)
	}
	s, err := store.NewBoltStore(filepath.Join(dir, "test.bolt"))
	if err != nil {
		t.Fatal("NewBoltStore:", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Save(idx); err != nil {
		t.Fatal("Save:", err)
	}
	return s, dir
}

// ── Correctness tests ──────────────────────────────────────────────────────

// TestIncrementalRename: renaming a function in file A should remove the old
// name and add the new one without touching file B's symbols.
func TestIncrementalRename(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"a.go": `package main
func OldName() {}
func Shared() {}
`,
		"b.go": `package main
func BOnly() {}
`,
	})

	fileA := filepath.Join(dir, "a.go")
	writeFile(t, fileA, `package main
func NewName() {}
func Shared() {}
`)

	knownVC, _ := s.GetKnownVC()
	syms, refs, err := indexer.IndexFile(fileA, knownVC)
	if err != nil {
		t.Fatal("IndexFile:", err)
	}
	if err := s.UpdateFile(fileA, 0, syms, refs); err != nil {
		t.Fatal("UpdateFile:", err)
	}

	// OldName must be gone.
	got, _ := s.FindSymbol("OldName")
	if len(got) != 0 {
		t.Errorf("OldName should be removed, got %v", got)
	}

	// NewName must exist.
	got, _ = s.FindSymbol("NewName")
	if len(got) == 0 {
		t.Error("NewName should exist after update")
	}

	// Shared stays (it's still in the new file A).
	got, _ = s.FindSymbol("Shared")
	if len(got) == 0 {
		t.Error("Shared should still exist")
	}

	// BOnly must be untouched.
	got, _ = s.FindSymbol("BOnly")
	if len(got) == 0 {
		t.Error("BOnly from file B should be untouched")
	}
}

// TestIncrementalRemoveSymbol: removing a function from a file cleans its entry
// from sym_by_name and sym_by_kind.
func TestIncrementalRemoveSymbol(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"a.go": `package main
func Alpha() {}
func Beta() {}
`,
	})

	fileA := filepath.Join(dir, "a.go")
	writeFile(t, fileA, `package main
func Alpha() {}
`)

	knownVC, _ := s.GetKnownVC()
	syms, refs, _ := indexer.IndexFile(fileA, knownVC)
	s.UpdateFile(fileA, 0, syms, refs)

	got, _ := s.FindSymbol("Beta")
	if len(got) != 0 {
		t.Errorf("Beta should be removed, got %v", got)
	}

	// Verify sym_by_kind also cleaned — ListSymbols(func) must not include Beta.
	funcs, _ := s.ListSymbols("func")
	for _, sym := range funcs {
		if sym.Name == "Beta" {
			t.Error("Beta still appears in ListSymbols(func)")
		}
	}
}

// TestIncrementalSameNameTwoFiles: two files sharing the same function name
// (different packages). Removing one file's contribution leaves the other intact.
func TestIncrementalSameNameTwoFiles(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"pkga/a.go": `package pkga
func Common() {}
`,
		"pkgb/b.go": `package pkgb
func Common() {}
`,
	})

	fileA := filepath.Join(dir, "pkga/a.go")

	// Remove pkga's Common.
	writeFile(t, fileA, `package pkga
`)
	knownVC, _ := s.GetKnownVC()
	syms, refs, _ := indexer.IndexFile(fileA, knownVC)
	s.UpdateFile(fileA, 0, syms, refs)

	// pkgb's Common must still exist.
	got, _ := s.FindSymbol("Common")
	if len(got) != 1 {
		t.Errorf("expected 1 Common from pkgb, got %d: %v", len(got), got)
	}
	if len(got) == 1 && got[0].Package != "pkgb" {
		t.Errorf("expected pkgb Common, got pkg=%s", got[0].Package)
	}
}

// TestRemoveFile: RemoveFile removes all symbols and refs from that file.
func TestRemoveFile(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"a.go": `package main
var MyVar = 1
func UsesVar() { _ = MyVar }
`,
		"b.go": `package main
func BFunc() {}
`,
	})

	fileA := filepath.Join(dir, "a.go")
	if err := s.RemoveFile(fileA); err != nil {
		t.Fatal("RemoveFile:", err)
	}

	// MyVar gone.
	got, _ := s.FindSymbol("MyVar")
	if len(got) != 0 {
		t.Errorf("MyVar should be removed, got %v", got)
	}
	// UsesVar gone.
	got, _ = s.FindSymbol("UsesVar")
	if len(got) != 0 {
		t.Errorf("UsesVar should be removed, got %v", got)
	}
	// Refs to MyVar gone.
	refs, _ := s.FindRefs("MyVar")
	if len(refs) != 0 {
		t.Errorf("refs to MyVar should be removed, got %v", refs)
	}
	// BFunc untouched.
	got, _ = s.FindSymbol("BFunc")
	if len(got) == 0 {
		t.Error("BFunc from file B should still exist")
	}
}

// TestIncrementalRefsUpdated: after updating a file that calls a function,
// the call sites (refs) should reflect the new source.
func TestIncrementalRefsUpdated(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"a.go": `package main
func Target() {}
func Caller() { Target() }
`,
	})

	fileA := filepath.Join(dir, "a.go")
	// Remove the call to Target from Caller.
	writeFile(t, fileA, `package main
func Target() {}
func Caller() {}
`)
	knownVC, _ := s.GetKnownVC()
	syms, refs, _ := indexer.IndexFile(fileA, knownVC)
	s.UpdateFile(fileA, 0, syms, refs)

	// refs to Target should now be empty (no call site).
	refs2, _ := s.FindRefs("Target")
	for _, r := range refs2 {
		if r.InSymbol == "Caller" {
			t.Errorf("stale ref: Caller still calls Target: %v", r)
		}
	}

	// calls_from Caller should be empty.
	calls, _ := s.CallsFrom("Caller")
	if len(calls) != 0 {
		t.Errorf("Caller should have no calls, got %v", calls)
	}
}

// TestIncrementalAddFile: a brand-new file with no prior tracking entry is
// handled correctly by UpdateFile (removeFileTx on unknown path is a no-op).
func TestIncrementalAddFile(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"existing.go": `package main
func Existing() {}
`,
	})

	newFile := filepath.Join(dir, "new.go")
	writeFile(t, newFile, `package main
func Brand() {}
`)
	knownVC, _ := s.GetKnownVC()
	syms, refs, _ := indexer.IndexFile(newFile, knownVC)
	if err := s.UpdateFile(newFile, 0, syms, refs); err != nil {
		t.Fatal("UpdateFile new file:", err)
	}

	got, _ := s.FindSymbol("Brand")
	if len(got) == 0 {
		t.Error("Brand from new file should exist")
	}
	// Existing untouched.
	got, _ = s.FindSymbol("Existing")
	if len(got) == 0 {
		t.Error("Existing should still exist")
	}
}

// TestCrossFileRefCascade: verifies FilesReferencingSymbols finds the correct
// dependent files so the caller (MCP watcher) knows what to re-index.
func TestCrossFileRefCascade(t *testing.T) {
	s, dir := setupIndex(t, map[string]string{
		"lib.go": `package main
func Target() {}
`,
		"consumer1.go": `package main
func C1() { Target() }
`,
		"consumer2.go": `package main
func C2() { Target() }
`,
		"unrelated.go": `package main
func Unrelated() {}
`,
	})

	libFile := filepath.Join(dir, "lib.go")

	// Get symbols lib.go contributes.
	symNames, err := s.GetFileSymNames(libFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(symNames) == 0 {
		t.Fatal("expected lib.go to have symbols")
	}

	// Find files referencing lib.go's symbols.
	affected, err := s.FilesReferencingSymbols(symNames)
	if err != nil {
		t.Fatal(err)
	}

	affectedSet := map[string]bool{}
	for _, f := range affected {
		affectedSet[f] = true
	}

	c1 := filepath.Join(dir, "consumer1.go")
	c2 := filepath.Join(dir, "consumer2.go")
	unrelated := filepath.Join(dir, "unrelated.go")

	if !affectedSet[c1] {
		t.Error("consumer1.go should be in affected set")
	}
	if !affectedSet[c2] {
		t.Error("consumer2.go should be in affected set")
	}
	if affectedSet[unrelated] {
		t.Error("unrelated.go should NOT be in affected set")
	}
	if affectedSet[libFile] {
		t.Error("lib.go itself should NOT be in affected set")
	}
}

// ── Benchmarks ─────────────────────────────────────────────────────────────

// BenchmarkFullRebuild measures full IndexDir + Save on the codeindex project.
func BenchmarkFullRebuild(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "bench.bolt")
	s, err := store.NewBoltStore(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx, err := indexer.IndexDir("../")
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Save(idx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIncrementalUpdate measures single-file IndexFile + UpdateFile.
func BenchmarkIncrementalUpdate(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "bench.bolt")
	s, err := store.NewBoltStore(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	// Seed a full index first.
	idx, err := indexer.IndexDir("../")
	if err != nil {
		b.Fatal(err)
	}
	if err := s.Save(idx); err != nil {
		b.Fatal(err)
	}

	// Use the largest file (mcp/server.go) as the update target.
	target := "../mcp/server.go"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		knownVC, err := s.GetKnownVC()
		if err != nil {
			b.Fatal(err)
		}
		syms, refs, err := indexer.IndexFile(target, knownVC)
		if err != nil {
			b.Fatal(err)
		}
		if err := s.UpdateFile(target, 0, syms, refs); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetKnownVC measures the cost of loading var/const names.
func BenchmarkGetKnownVC(b *testing.B) {
	dbPath := filepath.Join(b.TempDir(), "bench.bolt")
	s, err := store.NewBoltStore(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	idx, _ := indexer.IndexDir("../")
	s.Save(idx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.GetKnownVC()
	}
}
