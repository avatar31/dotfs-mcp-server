// Package model defines the wire and storage schema shared by the parser,
// cache and MCP tool layers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Language classifies the runtime environment a symbol belongs to. The MCP
// client uses it to pick syntax-specific markdown fences and linting rules.
type Language string

// Supported language classifications.
const (
	LanguageC  Language = "c"
	LanguageGo Language = "go"
)

// Valid reports whether the language tag is one of the supported values.
func (l Language) Valid() bool {
	return l == LanguageC || l == LanguageGo
}

// SymbolType is the closed enumeration of indexable declaration kinds.
type SymbolType string

// Every declaration kind captured by the Phase 2 extraction pipeline.
const (
	SymbolFunction      SymbolType = "function"
	SymbolMethod        SymbolType = "method"
	SymbolStruct        SymbolType = "struct"
	SymbolInterface     SymbolType = "interface"
	SymbolMacro         SymbolType = "macro"
	SymbolMacroFunction SymbolType = "macro_function"
	SymbolEnum          SymbolType = "enum"
	SymbolTypedef       SymbolType = "typedef"
	SymbolConstant      SymbolType = "constant"
	SymbolTypeAlias     SymbolType = "type_alias"
)

var symbolTypeSet = map[SymbolType]struct{}{
	SymbolFunction:      {},
	SymbolMethod:        {},
	SymbolStruct:        {},
	SymbolInterface:     {},
	SymbolMacro:         {},
	SymbolMacroFunction: {},
	SymbolEnum:          {},
	SymbolTypedef:       {},
	SymbolConstant:      {},
	SymbolTypeAlias:     {},
}

// Symbol type groupings used by the specialised MCP retrieval tools.
var (
	// CallableTypes back global_codebase_search.
	CallableTypes = []SymbolType{SymbolFunction, SymbolMethod}
	// TypeDefinitionTypes back get_type_definition.
	TypeDefinitionTypes = []SymbolType{SymbolStruct, SymbolInterface, SymbolTypedef, SymbolTypeAlias, SymbolEnum}
	// MacroConstTypes back lookup_macro_or_const.
	MacroConstTypes = []SymbolType{SymbolMacro, SymbolMacroFunction, SymbolConstant, SymbolEnum}
)

// Valid reports whether t is a member of the closed symbol_type enumeration.
func (t SymbolType) Valid() bool {
	_, ok := symbolTypeSet[t]
	return ok
}

// SymbolTypes returns every allowed symbol_type value in stable order.
func SymbolTypes() []SymbolType {
	out := make([]SymbolType, 0, len(symbolTypeSet))
	for t := range symbolTypeSet {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ParseSymbolType validates and normalises a caller-supplied filter value.
func ParseSymbolType(raw string) (SymbolType, error) {
	t := SymbolType(strings.ToLower(strings.TrimSpace(raw)))
	if !t.Valid() {
		names := SymbolTypes()
		labels := make([]string, len(names))
		for i, n := range names {
			labels[i] = string(n)
		}
		return "", fmt.Errorf("unsupported symbol_type %q, want one of: %s", raw, strings.Join(labels, ", "))
	}
	return t, nil
}

// SymbolRecord is the JSON value persisted under the primary
// "sym:<repo>:<file_path>:<symbol_type>:<name>:<offset>" key. The field set and
// JSON names are contractual: the LLM client consumes this document verbatim.
type SymbolRecord struct {
	RepoName      string     `json:"repo_name"`
	FilePath      string     `json:"file_path"`
	Language      Language   `json:"language"`
	SymbolType    SymbolType `json:"symbol_type"`
	Name          string     `json:"name"`
	Aliases       []string   `json:"aliases,omitempty"`
	ParentScope   string     `json:"parent_scope"`
	StartByte     int        `json:"start_byte"`
	EndByte       int        `json:"end_byte"`
	StartLine     int        `json:"start_line"`
	EndLine       int        `json:"end_line"`
	Documentation string     `json:"documentation"`
	Signature     string     `json:"signature"`
	SourceCode    string     `json:"source_code"`
}

// Validate guards against writing structurally incomplete records into cache.
func (r SymbolRecord) Validate() error {
	if strings.TrimSpace(r.RepoName) == "" {
		return fmt.Errorf("record is missing repo_name")
	}
	if strings.TrimSpace(r.FilePath) == "" {
		return fmt.Errorf("record is missing file_path")
	}
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("record is missing name")
	}
	if !r.Language.Valid() {
		return fmt.Errorf("record has unsupported language %q, want %q or %q", r.Language, LanguageC, LanguageGo)
	}
	if !r.SymbolType.Valid() {
		return fmt.Errorf("record has unsupported symbol_type %q", r.SymbolType)
	}
	if strings.TrimSpace(r.SourceCode) == "" {
		return fmt.Errorf("record is missing source_code")
	}
	if r.StartByte < 0 || r.EndByte < r.StartByte {
		return fmt.Errorf("record has an inverted byte range [%d,%d)", r.StartByte, r.EndByte)
	}
	if r.StartLine <= 0 || r.EndLine < r.StartLine {
		return fmt.Errorf("record has an inverted line range [%d,%d]", r.StartLine, r.EndLine)
	}
	return nil
}

// Fingerprint returns a stable digest of the record used to detect a structural
// delta before issuing a BadgerDB write.
func (r SymbolRecord) Fingerprint() string {
	h := sha256.New()
	parts := []string{
		r.RepoName, r.FilePath, string(r.Language), string(r.SymbolType),
		r.Name, r.ParentScope, r.Documentation, r.Signature, r.SourceCode,
	}
	parts = append(parts, r.Aliases...)
	for _, part := range parts {
		// The length prefix makes the concatenation unambiguous.
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	fmt.Fprintf(h, "%d:%d:%d:%d", r.StartByte, r.EndByte, r.StartLine, r.EndLine)
	return hex.EncodeToString(h.Sum(nil))
}
