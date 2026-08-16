package parser

import (
	"context"
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
	return nil, nil // TODO: implement Go AST extraction
}
