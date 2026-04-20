package indexer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	sitter "github.com/tree-sitter/go-tree-sitter"
	golang "github.com/tree-sitter/tree-sitter-go/bindings/go"
)

type Symbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // func, method, struct, interface, type, var, const
	Package   string `json:"pkg"`
	Receiver  string `json:"recv,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"sig,omitempty"`
}

type Reference struct {
	Symbol   string `json:"sym"`
	Kind     string `json:"kind"` // call, var_ref, const_ref
	File     string `json:"file"`
	Line     int    `json:"line"`
	InSymbol string `json:"in,omitempty"`
}

type Index struct {
	Symbols    []Symbol    `json:"symbols"`
	References []Reference `json:"refs"`
	FileCount  int         `json:"file_count"`
	IndexedAt  time.Time   `json:"indexed_at"`
}

var skipDirs = map[string]bool{
	"vendor": true, ".git": true, "node_modules": true, "testdata": true,
}

// Queries against Go grammar nodes.
const (
	// Captures top-level declarations we care about.
	qSymbols = `[
		(function_declaration) @node
		(method_declaration) @node
		(type_declaration) @node
		(const_declaration) @node
		(var_declaration) @node
	]`

	// Captures function/method name + body for ref extraction.
	qFuncBodies = `[
		(function_declaration name: (identifier) @name body: (block) @body)
		(method_declaration   name: (field_identifier) @name body: (block) @body)
	]`

	// Captures function call names (simple and selector).
	qCalls = `[
		(call_expression function: (identifier) @name)
		(call_expression function: (selector_expression field: (field_identifier) @name))
	]`

	// Captures all identifiers (used to find var/const usages).
	qIdents = `(identifier) @name`
)

type indexer struct {
	parser  *sitter.Parser
	lang    *sitter.Language
	qSyms   *sitter.Query
	qBodies *sitter.Query
	qCalls  *sitter.Query
	qIdents *sitter.Query
}

func newIndexer() (*indexer, error) {
	lang := sitter.NewLanguage(golang.Language())
	p := sitter.NewParser()
	p.SetLanguage(lang)

	compile := func(src string) (*sitter.Query, error) {
		q, qErr := sitter.NewQuery(lang, src)
		if qErr != nil {
			return nil, fmt.Errorf("query compile: %s", qErr.Message)
		}
		return q, nil
	}

	qSyms, err := compile(qSymbols)
	if err != nil {
		return nil, err
	}
	qBodies, err := compile(qFuncBodies)
	if err != nil {
		return nil, err
	}
	qCalls, err := compile(qCalls)
	if err != nil {
		return nil, err
	}
	qIdents, err := compile(qIdents)
	if err != nil {
		return nil, err
	}

	return &indexer{
		parser:  p,
		lang:    lang,
		qSyms:   qSyms,
		qBodies: qBodies,
		qCalls:  qCalls,
		qIdents: qIdents,
	}, nil
}

func (ix *indexer) close() {
	ix.parser.Close()
	ix.qSyms.Close()
	ix.qBodies.Close()
	ix.qCalls.Close()
	ix.qIdents.Close()
}

type goFile struct {
	path string
	src  []byte
	pkg  string
}

func IndexDir(dir string) (*Index, error) {
	ix, err := newIndexer()
	if err != nil {
		return nil, err
	}
	defer ix.close()

	var files []goFile
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", path, err)
			return nil
		}
		files = append(files, goFile{path: path, src: src, pkg: ix.packageName(src)})
		return nil
	})
	if err != nil {
		return nil, err
	}

	idx := &Index{IndexedAt: time.Now(), FileCount: len(files)}

	// Pass 1: extract all symbols from every file.
	for _, f := range files {
		idx.Symbols = append(idx.Symbols, ix.extractSymbols(f)...)
	}

	// Build a lookup of known package-level var/const names.
	knownVC := map[string]string{} // name → "var" | "const"
	for _, s := range idx.Symbols {
		if s.Kind == "var" || s.Kind == "const" {
			knownVC[s.Name] = s.Kind
		}
	}

	// Pass 2: extract references (calls + var/const usages) inside func bodies.
	for _, f := range files {
		idx.References = append(idx.References, ix.extractRefs(f, knownVC)...)
	}

	return idx, nil
}

// packageName parses src and returns the declared package name.
func (ix *indexer) packageName(src []byte) string {
	tree := ix.parser.Parse(src, nil)
	defer tree.Close()
	root := tree.RootNode()
	for i := uint(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(i)
		if child != nil && child.Kind() == "package_clause" {
			if id := child.NamedChild(0); id != nil {
				return id.Utf8Text(src)
			}
		}
	}
	return ""
}

// extractSymbols returns all top-level symbols in a file.
func (ix *indexer) extractSymbols(f goFile) []Symbol {
	tree := ix.parser.Parse(f.src, nil)
	defer tree.Close()

	var syms []Symbol
	qc := sitter.NewQueryCursor()
	defer qc.Close()

	matches := qc.Matches(ix.qSyms, tree.RootNode(), f.src)
	for match := matches.Next(); match != nil; match = matches.Next() {
		for _, cap := range match.Captures {
			node := &cap.Node
			switch node.Kind() {
			case "function_declaration":
				if s := ix.funcSymbol(node, f.src, f.path, f.pkg); s != nil {
					syms = append(syms, *s)
				}
			case "method_declaration":
				if s := ix.methodSymbol(node, f.src, f.path, f.pkg); s != nil {
					syms = append(syms, *s)
				}
			case "type_declaration":
				syms = append(syms, ix.typeSymbols(node, f.src, f.path, f.pkg)...)
			case "const_declaration":
				syms = append(syms, ix.valueSymbols(node, f.src, f.path, f.pkg, "const")...)
			case "var_declaration":
				syms = append(syms, ix.valueSymbols(node, f.src, f.path, f.pkg, "var")...)
			}
		}
	}
	return syms
}

// extractRefs returns all call references and var/const usages inside function bodies.
func (ix *indexer) extractRefs(f goFile, knownVC map[string]string) []Reference {
	tree := ix.parser.Parse(f.src, nil)
	defer tree.Close()

	var refs []Reference
	qc := sitter.NewQueryCursor()
	defer qc.Close()

	// Iterate over each function/method body.
	bodyMatches := qc.Matches(ix.qBodies, tree.RootNode(), f.src)
	for match := bodyMatches.Next(); match != nil; match = bodyMatches.Next() {
		var inSym string
		var bodyNode *sitter.Node

		for _, cap := range match.Captures {
			switch ix.qBodies.CaptureNames()[cap.Index] {
			case "name":
				inSym = cap.Node.Utf8Text(f.src)
			case "body":
				n := cap.Node
				bodyNode = &n
			}
		}

		// For methods, prepend receiver type to get "Receiver.MethodName".
		if bodyNode != nil {
			// The parent of body is the method_declaration; get receiver if present.
			parent := bodyNode.Parent()
			if parent != nil && parent.Kind() == "method_declaration" {
				if recv := receiverType(parent, f.src); recv != "" {
					inSym = recv + "." + inSym
				}
			}

			refs = append(refs, ix.refsInBody(bodyNode, f.src, f.path, inSym, knownVC)...)
		}
	}
	return refs
}

func (ix *indexer) refsInBody(body *sitter.Node, src []byte, path, inSym string, knownVC map[string]string) []Reference {
	var refs []Reference
	callPositions := map[uint]bool{}

	// Collect calls first.
	qc1 := sitter.NewQueryCursor()
	defer qc1.Close()
	callCaptures := qc1.Captures(ix.qCalls, body, src)
	for match, idx := callCaptures.Next(); match != nil; match, idx = callCaptures.Next() {
		node := match.Captures[idx].Node
		name := node.Utf8Text(src)
		pos := node.StartPosition()
		refs = append(refs, Reference{
			Symbol:   name,
			Kind:     "call",
			File:     path,
			Line:     int(pos.Row) + 1,
			InSymbol: inSym,
		})
		callPositions[node.StartByte()] = true
	}

	// Collect var/const identifier usages (skip call positions to avoid double-count).
	if len(knownVC) > 0 {
		qc2 := sitter.NewQueryCursor()
		defer qc2.Close()
		identCaptures := qc2.Captures(ix.qIdents, body, src)
		for match, idx := identCaptures.Next(); match != nil; match, idx = identCaptures.Next() {
			node := match.Captures[idx].Node
			name := node.Utf8Text(src)
			kind, known := knownVC[name]
			if !known || callPositions[node.StartByte()] {
				continue
			}
			pos := node.StartPosition()
			refs = append(refs, Reference{
				Symbol:   name,
				Kind:     kind + "_ref",
				File:     path,
				Line:     int(pos.Row) + 1,
				InSymbol: inSym,
			})
		}
	}

	return refs
}

func (ix *indexer) funcSymbol(node *sitter.Node, src []byte, path, pkg string) *Symbol {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	pos := node.StartPosition()
	end := node.EndPosition()
	name := nameNode.Utf8Text(src)
	return &Symbol{
		Name:      name,
		Kind:      "func",
		Package:   pkg,
		File:      path,
		Line:      int(pos.Row) + 1,
		EndLine:   int(end.Row) + 1,
		Signature: buildFuncSig(node, src, "", name),
	}
}

func (ix *indexer) methodSymbol(node *sitter.Node, src []byte, path, pkg string) *Symbol {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return nil
	}
	pos := node.StartPosition()
	end := node.EndPosition()
	recv := receiverType(node, src)
	name := nameNode.Utf8Text(src)
	return &Symbol{
		Name:      name,
		Kind:      "method",
		Package:   pkg,
		Receiver:  recv,
		File:      path,
		Line:      int(pos.Row) + 1,
		EndLine:   int(end.Row) + 1,
		Signature: buildFuncSig(node, src, recv, name),
	}
}

func (ix *indexer) typeSymbols(node *sitter.Node, src []byte, path, pkg string) []Symbol {
	var syms []Symbol
	for i := uint(0); i < node.NamedChildCount(); i++ {
		spec := node.NamedChild(i)
		if spec == nil || spec.Kind() != "type_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		typeNode := spec.ChildByFieldName("type")
		if nameNode == nil {
			continue
		}
		pos := spec.StartPosition()
		end := spec.EndPosition()
		kind := "type"
		if typeNode != nil {
			switch typeNode.Kind() {
			case "struct_type":
				kind = "struct"
			case "interface_type":
				kind = "interface"
			}
		}
		syms = append(syms, Symbol{
			Name:    nameNode.Utf8Text(src),
			Kind:    kind,
			Package: pkg,
			File:    path,
			Line:    int(pos.Row) + 1,
			EndLine: int(end.Row) + 1,
		})
	}
	return syms
}

func (ix *indexer) valueSymbols(node *sitter.Node, src []byte, path, pkg, kind string) []Symbol {
	var syms []Symbol
	for i := uint(0); i < node.NamedChildCount(); i++ {
		spec := node.NamedChild(i) // const_spec or var_spec
		if spec == nil {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		pos := nameNode.StartPosition()

		// Build a readable signature.
		sig := kind + " " + nameNode.Utf8Text(src)
		if typeNode := spec.ChildByFieldName("type"); typeNode != nil {
			sig += " " + typeNode.Utf8Text(src)
		}
		if valNode := spec.ChildByFieldName("value"); valNode != nil {
			sig += " = " + valNode.Utf8Text(src)
		}

		syms = append(syms, Symbol{
			Name:      nameNode.Utf8Text(src),
			Kind:      kind,
			Package:   pkg,
			File:      path,
			Line:      int(pos.Row) + 1,
			EndLine:   int(pos.Row) + 1,
			Signature: sig,
		})
	}
	return syms
}

// receiverType extracts the base type name from a method_declaration's receiver.
func receiverType(methodNode *sitter.Node, src []byte) string {
	recv := methodNode.ChildByFieldName("receiver") // parameter_list
	if recv == nil {
		return ""
	}
	pd := recv.NamedChild(0) // parameter_declaration
	if pd == nil {
		return ""
	}
	typeNode := pd.ChildByFieldName("type")
	if typeNode == nil {
		return ""
	}
	switch typeNode.Kind() {
	case "type_identifier":
		return typeNode.Utf8Text(src)
	case "pointer_type":
		inner := typeNode.NamedChild(0)
		if inner != nil {
			return inner.Utf8Text(src)
		}
	}
	return ""
}

func buildFuncSig(node *sitter.Node, src []byte, recv, name string) string {
	var b strings.Builder
	b.WriteString("func ")
	if recv != "" {
		b.WriteString("(")
		b.WriteString(recv)
		b.WriteString(") ")
	}
	b.WriteString(name)
	if params := node.ChildByFieldName("parameters"); params != nil {
		b.WriteString(params.Utf8Text(src))
	}
	if result := node.ChildByFieldName("result"); result != nil {
		b.WriteString(" ")
		b.WriteString(result.Utf8Text(src))
	}
	return b.String()
}
