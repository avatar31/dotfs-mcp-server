// Package parser implements the dual AST engine: Go source is handled by the
// standard library (go/token, go/parser, go/ast) while C source is handled by
// the Tree-sitter C grammar. A registry routes each file to the correct engine
// purely by its extension.
//
// Both engines emit the same neutral Symbol value, so the indexer never has to
// branch on language when populating the cache.
package parser

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// Symbol is one extracted declaration: a function, macro, struct, interface,
// typedef, enum or constant.
type Symbol struct {
	// Name is the identifier used to build the primary and secondary keys.
	Name string
	// Aliases are additional lookup identifiers, e.g. "Server.Handle" for a Go
	// method so both plain and receiver-qualified stack-trace symbols resolve.
	Aliases []string
	// Type is the closed-enumeration declaration kind.
	Type model.SymbolType
	// ParentScope names the enclosing declaration: the receiver type of a Go
	// method, the enum owning a C enumerator, or the const block of a Go
	// constant. Empty for file-level declarations.
	ParentScope string
	// Signature is the one-line declaration header shown before the body.
	Signature string
	// Documentation is the boilerplate-free comment block attached to the
	// declaration (comment markers already stripped).
	Documentation string
	// SourceCode is the exact byte scope of the declaration.
	SourceCode string
	// Language classifies the extraction engine that produced this record.
	Language model.Language
	// StartByte / EndByte are the isolated byte offsets inside the source file.
	StartByte int
	EndByte   int
	// StartLine / EndLine are 1-based inclusive line numbers.
	StartLine int
	EndLine   int
}

// Engine is a language-specific structural extractor.
type Engine interface {
	// Language returns the classification tag written to the cache.
	Language() model.Language
	// Extensions lists the lowercase file extensions handled by the engine.
	Extensions() []string
	// Parse extracts every top-level declaration from src.
	Parse(ctx context.Context, path string, src []byte) ([]Symbol, error)
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

// cleanComment strips C/Go comment markers and uniform indentation so the cache
// stores boilerplate-free documentation text.
func cleanComment(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/*") {
		raw = strings.TrimPrefix(raw, "/*")
		raw = strings.TrimSuffix(raw, "*/")
	}

	lines := strings.Split(raw, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "///"):
			line = strings.TrimPrefix(line, "///")
		case strings.HasPrefix(line, "//"):
			line = strings.TrimPrefix(line, "//")
		case strings.HasPrefix(line, "*/"):
			line = strings.TrimPrefix(line, "*/")
		case strings.HasPrefix(line, "*"):
			line = strings.TrimPrefix(line, "*")
		}
		cleaned = append(cleaned, strings.TrimSpace(line))
	}

	// Drop leading and trailing blank lines while preserving internal spacing.
	start, end := 0, len(cleaned)
	for start < end && cleaned[start] == "" {
		start++
	}
	for end > start && cleaned[end-1] == "" {
		end--
	}
	return strings.Join(cleaned[start:end], "\n")
}

// firstLine collapses a declaration to its opening line, which is what the
// "signature" field of a record carries.
func firstLine(src string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(src), "\n")
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), "{"))
}

// appendSection attaches a structured extraction summary (embedded types, struct
// tags, interface methods) underneath the human-written documentation.
func appendSection(doc, title string, entries []string) string {
	if len(entries) == 0 {
		return doc
	}
	section := title + ": " + strings.Join(entries, ", ")
	if strings.TrimSpace(doc) == "" {
		return section
	}
	return doc + "\n\n" + section
}
