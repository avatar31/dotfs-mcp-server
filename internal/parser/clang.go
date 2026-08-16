package parser

import (
	"bytes"
	"context"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// CEngine extracts C function definitions with the Tree-sitter C grammar.
//
// Tree-sitter parser handles are not safe for concurrent use, so a fresh parser
// is allocated (and explicitly closed) for every Parse call. The grammar
// pointer itself is immutable and shared.
type CEngine struct{}

// NewCEngine returns the C extraction engine.
func NewCEngine() *CEngine { return &CEngine{} }

// Language implements Engine.
func (*CEngine) Language() model.Language { return model.LanguageC }

// Extensions implements Engine.
func (*CEngine) Extensions() []string { return []string{".c", ".h"} }

// Prefilter implements Engine: a translation unit without a parenthesis cannot
// contain a function definition.
func (*CEngine) Prefilter(src []byte) bool {
	return bytes.Contains(src, []byte("("))
}

// Parse builds a Tree-sitter syntax tree and isolates every node typed
// "function_definition" along with the contiguous comment block preceding it.
//
// Because only genuine function_definition nodes are harvested, occurrences of
// a symbol inside string literals, macros or plain prints are structurally
// filtered out.
func (e *CEngine) Parse(ctx context.Context, path string, src []byte) ([]Function, error) {
	return nil, nil // TODO: implement Tree-sitter C extraction
}
