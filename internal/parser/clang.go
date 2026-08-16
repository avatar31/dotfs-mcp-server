package parser

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tsc "github.com/smacker/go-tree-sitter/c"

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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(src) > math.MaxUint32 {
		return nil, fmt.Errorf("c file %q exceeds the tree-sitter byte limit", path)
	}

	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(tsc.GetLanguage())

	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, fmt.Errorf("tree-sitter parse %q: %w", path, err)
	}
	defer tree.Close()

	var out []Function
	walkC(tree.RootNode(), func(node *sitter.Node) bool {
		if node.Type() != "function_definition" {
			return true // keep descending: definitions may sit inside #ifdef blocks
		}

		name := declaratorName(node.ChildByFieldName("declarator"), src)
		if name == "" {
			return false
		}

		start, end := int(node.StartByte()), int(node.EndByte())
		if start < 0 || end > len(src) || start >= end {
			return false
		}

		out = append(out, Function{
			Name:          name,
			Documentation: precedingComments(node, src),
			SourceCode:    string(src[start:end]),
			Language:      model.LanguageC,
			StartByte:     start,
			EndByte:       end,
		})
		return false // do not descend into the body of an extracted function
	})

	return out, nil
}

// walkC performs a depth-first traversal, descending only while visit returns
// true for the current node.
func walkC(node *sitter.Node, visit func(*sitter.Node) bool) {
	if node == nil {
		return
	}
	if !visit(node) {
		return
	}
	for i := 0; i < int(node.NamedChildCount()); i++ {
		walkC(node.NamedChild(i), visit)
	}
}

// declaratorName resolves the identifier of a (possibly pointer, array or
// parenthesised) C declarator chain while ignoring parameter identifiers.
func declaratorName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier", "field_identifier", "type_identifier":
		return node.Content(src)
	}

	if child := node.ChildByFieldName("declarator"); child != nil {
		if name := declaratorName(child, src); name != "" {
			return name
		}
	}

	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "parameter_list", "compound_statement", "attribute_specifier":
			continue
		}
		if name := declaratorName(child, src); name != "" {
			return name
		}
	}
	return ""
}

// precedingComments captures the block of comment nodes immediately above a
// function definition. A blank line terminates the block so unrelated file
// banners are not attributed to the function.
func precedingComments(node *sitter.Node, src []byte) string {
	var blocks []string
	cursor := node.PrevSibling()
	next := node

	for cursor != nil && cursor.Type() == "comment" {
		gap := src[cursor.EndByte():next.StartByte()]
		if bytes.Count(gap, []byte("\n")) > 1 {
			break // blank line: the comment belongs to something else
		}
		blocks = append([]string{cursor.Content(src)}, blocks...)
		next = cursor
		cursor = cursor.PrevSibling()
	}

	if len(blocks) == 0 {
		return ""
	}
	return cleanComment(strings.Join(blocks, "\n"))
}
