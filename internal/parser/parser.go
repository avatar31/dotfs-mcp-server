// Package parser implements the dual AST engine: Go source is handled by the
// standard library (go/token, go/parser, go/ast) while C source is handled by
// the Tree-sitter C grammar. A registry routes each file to the correct engine
// purely by its extension.
package parser

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// Function is one extracted top-level semantic block.
type Function struct {
	// Name is the primary cache key (BadgerDB key "func:<Name>").
	Name string
	// Aliases are additional lookup keys, e.g. "Server.Handle" for a Go method
	// so that both plain and receiver-qualified stack-trace symbols resolve.
	Aliases []string
	// Documentation is the boilerplate-free comment block attached to the
	// function (comment markers already stripped).
	Documentation string
	// SourceCode is the exact byte scope of the function definition.
	SourceCode string
	// Language classifies the extraction engine that produced this record.
	Language model.Language
	// StartByte / EndByte are the isolated byte offsets inside the source file.
	StartByte int
	EndByte   int
}

// Engine is a language-specific structural extractor.
type Engine interface {
	// Language returns the classification tag written to the cache.
	Language() model.Language
	// Extensions lists the lowercase file extensions handled by the engine.
	Extensions() []string
	// Parse extracts every top-level function definition from src.
	Parse(ctx context.Context, path string, src []byte) ([]Function, error)
	// Prefilter is a cheap language-aware rejection test executed in Phase 1
	// of the "Filter-Then-Parse" algorithm when no target symbol is supplied.
	Prefilter(src []byte) bool
}

// Registry routes files to engines by extension.
type Registry struct {
	byExt map[string]Engine
}

// NewRegistry builds a registry from the supplied engines.
func NewRegistry(engines ...Engine) *Registry {
	r := &Registry{byExt: make(map[string]Engine)}
	for _, e := range engines {
		for _, ext := range e.Extensions() {
			r.byExt[strings.ToLower(ext)] = e
		}
	}
	return r
}

// NewDefaultRegistry wires the Go and C engines mandated by the specification.
func NewDefaultRegistry() *Registry {
	return NewRegistry(NewGoEngine(), NewCEngine())
}

// For resolves the engine responsible for path, if any.
func (r *Registry) For(path string) (Engine, bool) {
	e, ok := r.byExt[strings.ToLower(filepath.Ext(path))]
	return e, ok
}

// Supports reports whether path is parseable by any registered engine.
func (r *Registry) Supports(path string) bool {
	_, ok := r.For(path)
	return ok
}

// Extensions returns the sorted set of handled extensions (diagnostics only).
func (r *Registry) Extensions() []string {
	out := make([]string, 0, len(r.byExt))
	for ext := range r.byExt {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}
