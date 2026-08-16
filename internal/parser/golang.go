package parser

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// GoEngine extracts *ast.FuncDecl nodes using Go's native standard library.
type GoEngine struct{}

// NewGoEngine returns the Go extraction engine.
func NewGoEngine() *GoEngine { return &GoEngine{} }

// Language implements Engine.
func (*GoEngine) Language() model.Language { return model.LanguageGo }

// Extensions implements Engine.
func (*GoEngine) Extensions() []string { return []string{".go"} }

// Prefilter implements Engine: a Go file without the "func" keyword cannot
// declare a function, so it is rejected before the AST stage.
func (*GoEngine) Prefilter(src []byte) bool {
	return strings.Contains(string(src), "func")
}

// Parse builds the AST for a Go file and isolates every top-level function and
// method declaration together with its *ast.CommentGroup documentation.
func (e *GoEngine) Parse(ctx context.Context, path string, src []byte) ([]Function, error) {
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

	var out []Function
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}

		// fn.Pos() points at the "func" keyword, so the doc comment is
		// deliberately excluded from the isolated source slice.
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		if start < 0 || end > len(src) || start >= end {
			continue
		}

		f := Function{
			Name:          fn.Name.Name,
			Documentation: cleanComment(fn.Doc.Text()),
			SourceCode:    string(src[start:end]),
			Language:      model.LanguageGo,
			StartByte:     start,
			EndByte:       end,
		}
		if recv := receiverName(fn); recv != "" {
			// A stack trace may reference either "Handle" or "Server.Handle".
			f.Aliases = append(f.Aliases, recv+"."+fn.Name.Name)
		}
		out = append(out, f)
	}
	return out, nil
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
