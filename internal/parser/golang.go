package parser

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// GoEngine extracts every top-level declaration from a Go file using the
// standard library AST: functions, methods, structs, interfaces, type aliases,
// named types, constant blocks and exported package-level variables.
type GoEngine struct{}

// NewGoEngine returns the Go extraction engine.
func NewGoEngine() *GoEngine { return &GoEngine{} }

// Language implements Engine.
func (*GoEngine) Language() model.Language { return model.LanguageGo }

// Extensions implements Engine.
func (*GoEngine) Extensions() []string { return []string{".go"} }

// Prefilter implements Engine: a file without a package clause cannot contain
// any indexable Go declaration.
func (*GoEngine) Prefilter(src []byte) bool {
	return strings.Contains(string(src), "package ")
}

// goDecl carries the per-file state shared by the declaration visitors.
type goDecl struct {
	fset *token.FileSet
	src  []byte
}

// Parse builds the AST for a Go file and walks file.Decls with a type switch
// over *ast.FuncDecl and *ast.GenDecl, as mandated by the specification.
//
// The AST and its FileSet are function-local, so the whole graph becomes
// garbage immediately after Parse returns; no per-file AST is ever retained.
func (e *GoEngine) Parse(ctx context.Context, path string, src []byte) ([]Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	// ParseComments keeps doc groups attached; SkipObjectResolution avoids the
	// expensive (and here useless) identifier resolution pass.
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse go file %q: %w", path, err)
	}

	g := goDecl{fset: fset, src: src}
	var out []Symbol
	for _, decl := range file.Decls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if sym, ok := g.function(d); ok {
				out = append(out, sym)
			}
		case *ast.GenDecl:
			out = append(out, g.generic(d)...)
		}
	}
	return out, nil
}

// span converts an AST range into byte offsets and 1-based line numbers,
// rejecting anything that falls outside the source buffer.
func (g goDecl) span(from, to token.Pos) (startByte, endByte, startLine, endLine int, ok bool) {
	sp, ep := g.fset.Position(from), g.fset.Position(to)
	if sp.Offset < 0 || ep.Offset > len(g.src) || sp.Offset >= ep.Offset {
		return 0, 0, 0, 0, false
	}
	return sp.Offset, ep.Offset, sp.Line, ep.Line, true
}

// function records an *ast.FuncDecl as either a function or a method.
func (g goDecl) function(fn *ast.FuncDecl) (Symbol, bool) {
	if fn.Name == nil {
		return Symbol{}, false
	}
	// fn.Pos() points at the "func" keyword, so the doc comment is deliberately
	// excluded from the isolated source slice.
	sb, eb, sl, el, ok := g.span(fn.Pos(), fn.End())
	if !ok {
		return Symbol{}, false
	}

	sym := Symbol{
		Name:          fn.Name.Name,
		Type:          model.SymbolFunction,
		Documentation: cleanComment(fn.Doc.Text()),
		SourceCode:    string(g.src[sb:eb]),
		Language:      model.LanguageGo,
		StartByte:     sb,
		EndByte:       eb,
		StartLine:     sl,
		EndLine:       el,
	}

	sigEnd := fn.End()
	if fn.Body != nil {
		sigEnd = fn.Body.Lbrace
	}
	if s, e, _, _, sok := g.span(fn.Pos(), sigEnd); sok {
		sym.Signature = firstLine(string(g.src[s:e]))
	}

	if recv := receiverName(fn); recv != "" {
		sym.Type = model.SymbolMethod
		sym.ParentScope = recv
		// A stack trace may reference either "Handle" or "Server.Handle".
		sym.Aliases = append(sym.Aliases, recv+"."+fn.Name.Name)
	}
	return sym, true
}

// generic dispatches an *ast.GenDecl on its token: TYPE, CONST or VAR.
func (g goDecl) generic(decl *ast.GenDecl) []Symbol {
	switch decl.Tok {
	case token.TYPE:
		var out []Symbol
		for _, spec := range decl.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil {
				continue
			}
			if sym, ok := g.typeSpec(decl, ts); ok {
				out = append(out, sym)
			}
		}
		return out
	case token.CONST, token.VAR:
		return g.valueSpecs(decl)
	default:
		return nil
	}
}

// typeSpec records a struct, interface, type alias or defined type.
func (g goDecl) typeSpec(decl *ast.GenDecl, ts *ast.TypeSpec) (Symbol, bool) {
	// A single-spec declaration owns the "type" keyword, so the slice reads as
	// valid standalone Go; grouped specs get the keyword prepended below.
	from := ts.Pos()
	if !decl.Lparen.IsValid() {
		from = decl.Pos()
	}
	sb, eb, sl, el, ok := g.span(from, ts.End())
	if !ok {
		return Symbol{}, false
	}
	source := string(g.src[sb:eb])
	if !strings.HasPrefix(source, "type ") {
		source = "type " + source
	}

	doc := ts.Doc.Text()
	if strings.TrimSpace(doc) == "" {
		doc = decl.Doc.Text()
	}

	sym := Symbol{
		Name:          ts.Name.Name,
		ParentScope:   "",
		Documentation: cleanComment(doc),
		SourceCode:    source,
		Language:      model.LanguageGo,
		StartByte:     sb,
		EndByte:       eb,
		StartLine:     sl,
		EndLine:       el,
	}

	switch underlying := ts.Type.(type) {
	case *ast.StructType:
		sym.Type = model.SymbolStruct
		sym.Signature = "type " + ts.Name.Name + " struct"
		embedded, tags := structDetails(underlying)
		sym.Documentation = appendSection(sym.Documentation, "Embedded fields", embedded)
		sym.Documentation = appendSection(sym.Documentation, "Struct tags", tags)
	case *ast.InterfaceType:
		sym.Type = model.SymbolInterface
		sym.Signature = "type " + ts.Name.Name + " interface"
		methods, embedded := interfaceDetails(underlying)
		sym.Documentation = appendSection(sym.Documentation, "Methods", methods)
		sym.Documentation = appendSection(sym.Documentation, "Embedded interfaces", embedded)
	default:
		if ts.Assign.IsValid() {
			sym.Type = model.SymbolTypeAlias
			sym.Signature = "type " + ts.Name.Name + " = " + types.ExprString(ts.Type)
		} else {
			// A defined (non-alias) named type is Go's closest analogue of a
			// C typedef, so it shares the typedef symbol_type.
			sym.Type = model.SymbolTypedef
			sym.Signature = "type " + ts.Name.Name + " " + types.ExprString(ts.Type)
		}
	}
	return sym, true
}

// valueSpecs records every identifier of a const block and every exported
// package-level var. Each constant carries the whole "const (...)" block as its
// source so grouped iota enumerations stay readable in isolation.
func (g goDecl) valueSpecs(decl *ast.GenDecl) []Symbol {
	blockStart, blockEnd, _, _, ok := g.span(decl.Pos(), decl.End())
	if !ok {
		return nil
	}
	block := string(g.src[blockStart:blockEnd])
	keyword := "const"
	if decl.Tok == token.VAR {
		keyword = "var"
	}

	var out []Symbol
	for _, spec := range decl.Specs {
		vs, isValue := spec.(*ast.ValueSpec)
		if !isValue {
			continue
		}
		sb, eb, sl, el, spanOK := g.span(vs.Pos(), vs.End())
		if !spanOK {
			continue
		}

		doc := vs.Doc.Text()
		if strings.TrimSpace(doc) == "" {
			doc = decl.Doc.Text()
		}
		if strings.TrimSpace(doc) == "" {
			doc = vs.Comment.Text()
		}

		for _, ident := range vs.Names {
			if ident == nil || ident.Name == "_" {
				continue
			}
			// Unexported package variables are implementation detail noise; the
			// specification only asks for exported globals.
			if decl.Tok == token.VAR && !ast.IsExported(ident.Name) {
				continue
			}
			out = append(out, Symbol{
				Name:          ident.Name,
				Type:          model.SymbolConstant,
				ParentScope:   constBlockScope(decl, vs),
				Signature:     keyword + " " + firstLine(string(g.src[sb:eb])),
				Documentation: cleanComment(doc),
				SourceCode:    block,
				Language:      model.LanguageGo,
				StartByte:     sb,
				EndByte:       eb,
				StartLine:     sl,
				EndLine:       el,
			})
		}
	}
	return out
}

// constBlockScope names the grouped declaration a constant belongs to, using the
// first identifier of the block. It gives the LLM a stable handle for an
// otherwise anonymous iota enumeration.
func constBlockScope(decl *ast.GenDecl, self *ast.ValueSpec) string {
	if !decl.Lparen.IsValid() {
		return ""
	}
	for _, spec := range decl.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || len(vs.Names) == 0 {
			continue
		}
		if vs == self && len(decl.Specs) == 1 {
			return ""
		}
		return vs.Names[0].Name + " block"
	}
	return ""
}

// structDetails extracts embedded field types and JSON/YAML style struct tags.
func structDetails(st *ast.StructType) (embedded, tags []string) {
	if st.Fields == nil {
		return nil, nil
	}
	for _, field := range st.Fields.List {
		typeName := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			embedded = append(embedded, typeName)
		}
		if field.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			tag = field.Tag.Value
		}
		label := typeName
		if len(field.Names) > 0 {
			names := make([]string, 0, len(field.Names))
			for _, n := range field.Names {
				names = append(names, n.Name)
			}
			label = strings.Join(names, "/")
		}
		tags = append(tags, label+" `"+tag+"`")
	}
	return embedded, tags
}

// interfaceDetails extracts method signatures and embedded interface names.
func interfaceDetails(it *ast.InterfaceType) (methods, embedded []string) {
	if it.Methods == nil {
		return nil, nil
	}
	for _, field := range it.Methods.List {
		if len(field.Names) == 0 {
			embedded = append(embedded, types.ExprString(field.Type))
			continue
		}
		sig := types.ExprString(field.Type)
		sig = strings.TrimPrefix(sig, "func")
		for _, n := range field.Names {
			methods = append(methods, n.Name+sig)
		}
	}
	return methods, embedded
}

// receiverName renders the receiver type of a method, stripping pointer and
// generic decoration (e.g. "*Server[T]" becomes "Server").
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr:
			expr = t.X
		case *ast.IndexListExpr:
			expr = t.X
		case *ast.ParenExpr:
			expr = t.X
		case *ast.Ident:
			return t.Name
		case *ast.SelectorExpr:
			if t.Sel != nil {
				return t.Sel.Name
			}
			return ""
		default:
			return ""
		}
	}
}
