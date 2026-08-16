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

// CEngine extracts C declarations with the Tree-sitter C grammar: function
// definitions, header prototypes, object-like and function-like preprocessor
// macros, struct and union definitions, typedefs, enums and enumerators.
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

// cTokens are the cheapest possible proof that a translation unit declares
// something the engine can index.
var cTokens = [][]byte{
	[]byte("("), []byte("#define"), []byte("struct"),
	[]byte("union"), []byte("enum"), []byte("typedef"),
}

// Prefilter implements Engine.
func (*CEngine) Prefilter(src []byte) bool {
	for _, tok := range cTokens {
		if bytes.Contains(src, tok) {
			return true
		}
	}
	return false
}

// Parse builds a Tree-sitter syntax tree and harvests every indexable
// declaration node. The tree is closed before returning, so no AST pointer graph
// survives the call.
//
// Because only genuine declaration nodes are harvested, occurrences of a symbol
// inside string literals, call sites or plain prints are structurally filtered
// out.
func (e *CEngine) Parse(ctx context.Context, path string, src []byte) ([]Symbol, error) {
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

	var out []Symbol
	walkC(tree.RootNode(), func(node *sitter.Node) bool {
		switch node.Type() {
		case "function_definition":
			if sym, ok := cFunctionDefinition(node, src); ok {
				out = append(out, sym)
			}
			return false // never descend into a function body
		case "preproc_def":
			if sym, ok := cObjectMacro(node, src); ok {
				out = append(out, sym)
			}
			return false
		case "preproc_function_def":
			if sym, ok := cFunctionMacro(node, src); ok {
				out = append(out, sym)
			}
			return false
		case "struct_specifier", "union_specifier":
			if sym, ok := cRecord(node, src); ok {
				out = append(out, sym)
			}
			return true // a nested record may still declare inner types
		case "enum_specifier":
			out = append(out, cEnum(node, src)...)
			return false
		case "type_definition":
			out = append(out, cTypedefs(node, src)...)
			return true // descend so the underlying struct/enum is indexed too
		case "declaration":
			if sym, ok := cPrototype(node, src); ok {
				out = append(out, sym)
			}
			return true
		default:
			return true
		}
	})

	return out, nil
}

// nodeSpan converts a Tree-sitter node into byte offsets and 1-based lines,
// trimming trailing whitespace so a macro does not carry its line break.
func nodeSpan(node *sitter.Node, src []byte) (sb, eb, sl, el int, ok bool) {
	sb, eb = int(node.StartByte()), int(node.EndByte())
	if sb < 0 || eb > len(src) || sb >= eb {
		return 0, 0, 0, 0, false
	}
	for eb > sb && (src[eb-1] == '\n' || src[eb-1] == '\r' || src[eb-1] == ' ' || src[eb-1] == '\t') {
		eb--
	}
	if sb >= eb {
		return 0, 0, 0, 0, false
	}
	sl = int(node.StartPoint().Row) + 1
	// Recount the end line from the trimmed range: a preprocessor node reaches
	// into the following line, which would otherwise be reported as part of it.
	el = sl + bytes.Count(src[sb:eb], []byte("\n"))
	return sb, eb, sl, el, true
}

// newCSymbol fills in every field shared by the C symbol kinds.
func newCSymbol(node *sitter.Node, src []byte, name string, kind model.SymbolType) (Symbol, bool) {
	if strings.TrimSpace(name) == "" {
		return Symbol{}, false
	}
	sb, eb, sl, el, ok := nodeSpan(node, src)
	if !ok {
		return Symbol{}, false
	}
	source := string(src[sb:eb])
	return Symbol{
		Name:          name,
		Type:          kind,
		Signature:     firstLine(source),
		Documentation: docForNode(node, src),
		SourceCode:    source,
		Language:      model.LanguageC,
		StartByte:     sb,
		EndByte:       eb,
		StartLine:     sl,
		EndLine:       el,
	}, true
}

// cFunctionDefinition handles (function_definition).
func cFunctionDefinition(node *sitter.Node, src []byte) (Symbol, bool) {
	declarator := node.ChildByFieldName("declarator")
	sym, ok := newCSymbol(node, src, declaratorName(declarator, src), model.SymbolFunction)
	if !ok {
		return Symbol{}, false
	}
	if body := node.ChildByFieldName("body"); body != nil {
		if s, _, _, _, spanOK := nodeSpan(node, src); spanOK {
			sym.Signature = firstLine(string(src[s:int(body.StartByte())]))
		}
	}
	return sym, true
}

// cPrototype handles (declaration declarator: (function_declarator declarator:
// (identifier))), i.e. a header function prototype.
func cPrototype(node *sitter.Node, src []byte) (Symbol, bool) {
	fnDecl := node.ChildByFieldName("declarator")
	if fnDecl == nil || fnDecl.Type() != "function_declarator" {
		return Symbol{}, false
	}
	// The specification's query pins the inner declarator to a bare identifier,
	// which excludes function-pointer variables such as "int (*fp)(void);".
	inner := fnDecl.ChildByFieldName("declarator")
	if inner == nil || inner.Type() != "identifier" {
		return Symbol{}, false
	}
	sym, ok := newCSymbol(node, src, inner.Content(src), model.SymbolFunction)
	if !ok {
		return Symbol{}, false
	}
	sym.Signature = strings.TrimSuffix(sym.Signature, ";")
	return sym, true
}

// cObjectMacro handles (preproc_def name: (identifier) value: (_)?).
func cObjectMacro(node *sitter.Node, src []byte) (Symbol, bool) {
	name := node.ChildByFieldName("name")
	if name == nil {
		return Symbol{}, false
	}
	sym, ok := newCSymbol(node, src, name.Content(src), model.SymbolMacro)
	if !ok {
		return Symbol{}, false
	}
	sym.Signature = "#define " + sym.Name
	if value := node.ChildByFieldName("value"); value != nil {
		sym.Signature += " " + firstLine(value.Content(src))
	}
	return sym, true
}

// cFunctionMacro handles (preproc_function_def name: (identifier)
// parameters: (preproc_params) value: (_)?).
func cFunctionMacro(node *sitter.Node, src []byte) (Symbol, bool) {
	name := node.ChildByFieldName("name")
	if name == nil {
		return Symbol{}, false
	}
	sym, ok := newCSymbol(node, src, name.Content(src), model.SymbolMacroFunction)
	if !ok {
		return Symbol{}, false
	}
	sym.Signature = "#define " + sym.Name
	if params := node.ChildByFieldName("parameters"); params != nil {
		sym.Signature += params.Content(src)
	}
	return sym, true
}

// cRecord handles (struct_specifier|union_specifier name: (type_identifier)
// body: (field_declaration_list)). An anonymous record is skipped: the
// surrounding typedef already carries it.
func cRecord(node *sitter.Node, src []byte) (Symbol, bool) {
	name := node.ChildByFieldName("name")
	body := node.ChildByFieldName("body")
	if name == nil || body == nil || body.Type() != "field_declaration_list" {
		return Symbol{}, false
	}
	// Both struct and union map onto the struct symbol_type; the signature keeps
	// the distinction visible to the model.
	sym, ok := newCSymbol(node, src, name.Content(src), model.SymbolStruct)
	if !ok {
		return Symbol{}, false
	}
	keyword := "struct"
	if node.Type() == "union_specifier" {
		keyword = "union"
	}
	sym.Signature = keyword + " " + sym.Name
	sym.Documentation = appendSection(sym.Documentation, "Fields", recordFields(body, src))
	return sym, true
}

// recordFields lists the member names of a field_declaration_list, including
// function-pointer members that form a v-table.
func recordFields(body *sitter.Node, src []byte) []string {
	var fields []string
	for i := 0; i < int(body.NamedChildCount()); i++ {
		child := body.NamedChild(i)
		if child.Type() != "field_declaration" {
			continue
		}
		for j := 0; j < int(child.NamedChildCount()); j++ {
			sub := child.NamedChild(j)
			if sub == child.ChildByFieldName("type") {
				continue
			}
			if name := declaratorName(sub, src); name != "" {
				fields = append(fields, name)
			}
		}
	}
	return fields
}

// cEnum handles (enum_specifier name: (type_identifier)? body: (enumerator_list
// (enumerator name: (identifier))*)). It emits the enum itself plus one
// constant per enumerator, each carrying the whole enum block as context.
func cEnum(node *sitter.Node, src []byte) []Symbol {
	body := node.ChildByFieldName("body")
	if body == nil || body.Type() != "enumerator_list" {
		return nil
	}

	scope := ""
	if name := node.ChildByFieldName("name"); name != nil {
		scope = name.Content(src)
	} else if td := enclosingTypedefName(node, src); td != "" {
		// typedef enum { ... } status_t;  ->  enumerators belong to "status_t".
		scope = td
	}

	var out []Symbol
	if name := node.ChildByFieldName("name"); name != nil {
		if sym, ok := newCSymbol(node, src, name.Content(src), model.SymbolEnum); ok {
			sym.Signature = "enum " + sym.Name
			out = append(out, sym)
		}
	}

	block := ""
	if sb, eb, _, _, ok := nodeSpan(node, src); ok {
		block = string(src[sb:eb])
	}
	doc := docForNode(node, src)

	for i := 0; i < int(body.NamedChildCount()); i++ {
		enumerator := body.NamedChild(i)
		if enumerator.Type() != "enumerator" {
			continue
		}
		name := enumerator.ChildByFieldName("name")
		if name == nil {
			continue
		}
		sb, eb, sl, el, ok := nodeSpan(enumerator, src)
		if !ok {
			continue
		}
		enumDoc := docForNode(enumerator, src)
		if enumDoc == "" {
			enumDoc = doc
		}
		out = append(out, Symbol{
			Name:          name.Content(src),
			Type:          model.SymbolConstant,
			ParentScope:   scope,
			Signature:     firstLine(string(src[sb:eb])),
			Documentation: enumDoc,
			SourceCode:    block,
			Language:      model.LanguageC,
			StartByte:     sb,
			EndByte:       eb,
			StartLine:     sl,
			EndLine:       el,
		})
	}
	return out
}

// cTypedefs handles (type_definition declarator: (type_identifier)). A single
// type_definition may introduce several names.
func cTypedefs(node *sitter.Node, src []byte) []Symbol {
	underlying := node.ChildByFieldName("type")

	var out []Symbol
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child == underlying || child.Type() == "comment" {
			continue
		}
		name := declaratorName(child, src)
		if name == "" {
			continue
		}
		sym, ok := newCSymbol(node, src, name, model.SymbolTypedef)
		if !ok {
			continue
		}
		sym.Signature = "typedef " + name
		out = append(out, sym)
	}
	return out
}

// enclosingTypedefName resolves the name introduced by a typedef wrapping node.
func enclosingTypedefName(node *sitter.Node, src []byte) string {
	parent := node.Parent()
	if parent == nil || parent.Type() != "type_definition" {
		return ""
	}
	underlying := parent.ChildByFieldName("type")
	for i := 0; i < int(parent.NamedChildCount()); i++ {
		child := parent.NamedChild(i)
		if child == underlying || child.Type() == "comment" {
			continue
		}
		if name := declaratorName(child, src); name != "" {
			return name
		}
	}
	return ""
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

// docForNode collects the comment block attached to a declaration. It climbs a
// couple of wrapper levels so a comment above "typedef struct { ... } x_t;" is
// still attributed to the inner struct.
func docForNode(node *sitter.Node, src []byte) string {
	n := node
	for depth := 0; n != nil && depth < 3; depth++ {
		if doc := precedingComments(n, src); doc != "" {
			return doc
		}
		parent := n.Parent()
		if parent == nil || parent.Type() == "translation_unit" {
			break
		}
		n = parent
	}
	return ""
}

// precedingComments captures the block of comment nodes immediately above a
// declaration. A blank line terminates the block so unrelated file banners are
// not attributed to the declaration.
func precedingComments(node *sitter.Node, src []byte) string {
	var blocks []string
	cursor := node.PrevSibling()
	next := node

	for cursor != nil && cursor.Type() == "comment" {
		if int(cursor.EndByte()) > int(next.StartByte()) || int(next.StartByte()) > len(src) {
			break
		}
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
