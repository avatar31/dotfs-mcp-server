// Package lsp implements the subset of the Language Server Protocol required to
// drive clangd and gopls as supervised child processes: JSON-RPC 2.0 framing, a
// request/response client and a lazily initialised, per-repository daemon pool.
//
// Only the messages Phase 3 actually sends are modelled. Everything else the
// server pushes at us (progress, diagnostics, log messages) is answered
// generically so a daemon can never block waiting on the orchestrator.
package lsp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Method for LSP messages are defined in the LSP specification at
// https://microsoft.github.io/language-server-protocol/specifications/specification-current/#methods
const (
	MethodInitialize             = "initialize"
	MethodInitialized            = "initialized"
	MethodShutdown               = "shutdown"
	MethodExit                   = "exit"
	MethodDidOpen                = "textDocument/didOpen"
	MethodReferences             = "textDocument/references"
	MethodImplementation         = "textDocument/implementation"
	MethodDefinition             = "textDocument/definition"
	MethodTypeDefinition         = "textDocument/typeDefinition"
	MethodPrepareCallHierarchy   = "textDocument/prepareCallHierarchy"
	MethodIncomingCalls          = "callHierarchy/incomingCalls"
	MethodOutgoingCalls          = "callHierarchy/outgoingCalls"
	MethodPrepareTypeHierarchy   = "textDocument/prepareTypeHierarchy"
	MethodSupertypes             = "typeHierarchy/supertypes"
	MethodSubtypes               = "typeHierarchy/subtypes"
	methodCancelRequest          = "$/cancelRequest"
	methodWorkspaceConfiguration = "workspace/configuration"
)

// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#errorCodes
const (
	ErrCodeMethodNotFound = -32601
	ErrCodeRequestFailed  = -32803
)

// DocumentURI is an LSP document identifier, always a file:// URI here.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#uri
type DocumentURI string

// PathToURI converts an absolute filesystem path into a file:// URI.
//
// Each path segment is percent-encoded individually so separators survive, which
// matters for repositories checked out under directories containing spaces.
func PathToURI(path string) DocumentURI {
	slashed := filepath.ToSlash(path)

	// Windows volume names ("C:/x") must keep their colon but gain a leading
	// slash. On Unix the path already starts with "/".
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}

	var b strings.Builder
	b.WriteString("file://")
	for i, segment := range strings.Split(slashed, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(url.PathEscape(segment))
	}
	return DocumentURI(b.String())
}

// Path converts a file:// URI back into a local filesystem path.
func (u DocumentURI) Path() (string, error) {
	raw := string(u)
	if raw == "" {
		return "", fmt.Errorf("lsp: empty document URI")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("lsp: parse document URI %q: %w", raw, err)
	}
	// A language server always emits an absolute file:// URI. Anything else is
	// either a virtual document or garbage, and must not become a path.
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("lsp: unsupported URI scheme %q in %q", parsed.Scheme, raw)
	}
	if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
		return "", fmt.Errorf("lsp: refusing remote URI %q", raw)
	}

	path := parsed.Path
	if path == "" {
		return "", fmt.Errorf("lsp: document URI %q has no path", raw)
	}
	// "/C:/src/main.c" -> "C:/src/main.c".
	if len(path) > 2 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), nil
}

// SymbolKind is the LSP symbol classification enumeration.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#symbolKind
type SymbolKind int

// symbolKindNames maps the enumeration onto human-readable labels for the
// compacted payloads handed to the model.
var symbolKindNames = map[SymbolKind]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
	6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
	11: "interface", 12: "function", 13: "variable", 14: "constant", 15: "string",
	16: "number", 17: "boolean", 18: "array", 19: "object", 20: "key",
	21: "null", 22: "enum_member", 23: "struct", 24: "event", 25: "operator",
	26: "type_parameter",
}

// String renders the kind as a stable lower-case label.
func (k SymbolKind) String() string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return "unknown"
}

// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#initializeParams
type InitializeParams struct {
	ProcessID        int64              `json:"processId"`
	ClientInfo       ClientInfo         `json:"clientInfo"`
	RootPath         string             `json:"rootPath,omitempty"` // Deprecated
	RootURI          DocumentURI        `json:"rootUri"`            // Deprecated in favor of WorkspaceFolders
	Capabilities     ClientCapabilities `json:"capabilities"`
	WorkspaceFolders []*WorkspaceFolder `json:"workspaceFolders"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type ClientCapabilities struct {
	Workspace    *WorkspaceClientCapabilities    `json:"workspace,omitempty"`
	Window       *WindowClientCapabilities       `json:"window,omitempty"`
	TextDocument *TextDocumentClientCapabilities `json:"textDocument,omitempty"`
}

type WorkspaceClientCapabilities struct {
	WorkspaceFolders bool `json:"workspaceFolders"`
	Configuration    bool `json:"configuration"`
}

type WindowClientCapabilities struct {
	WorkDoneProgress bool `json:"workDoneProgress"`
}

type TextDocumentClientCapabilities struct {
	Synchronization map[string]any `json:"synchronization"`
	References      map[string]any `json:"references"`
	Definition      map[string]any `json:"definition"`
	TypeDefinition  map[string]any `json:"typeDefinition"`
	Implementation  map[string]any `json:"implementation"`
	CallHierarchy   map[string]any `json:"callHierarchy"`
	TypeHierarchy   map[string]any `json:"typeHierarchy"`
}

type WorkspaceFolder struct {
	URI  DocumentURI `json:"uri"`
	Name string      `json:"name"`
}

// Position is a zero-based line/character coordinate pair, exactly as defined by
// the protocol. Tool inputs are one-based and converted at the boundary.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location pins a range inside a document.
type Location struct {
	URI   DocumentURI `json:"uri"`
	Range Range       `json:"range"`
}

// LocationLink is the richer response shape some servers return for
// implementation and definition requests.
type LocationLink struct {
	TargetURI            DocumentURI `json:"targetUri"`
	TargetRange          Range       `json:"targetRange"`
	TargetSelectionRange Range       `json:"targetSelectionRange"`
}

// TextDocumentIdentifier references an open document.
type TextDocumentIdentifier struct {
	URI DocumentURI `json:"uri"`
}

// TextDocumentPositionParams is the base shape of every positional request.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceParams adds the declaration toggle to a positional request.
type ReferenceParams struct {
	TextDocumentPositionParams
	Context ReferenceContext `json:"context"`
}

// ReferenceContext controls whether the declaration itself is returned.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// CallHierarchyItem identifies one node of a call tree.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchyItem
type CallHierarchyItem struct {
	Name           string      `json:"name"`
	Kind           SymbolKind  `json:"kind"`
	Detail         string      `json:"detail,omitempty"`
	URI            DocumentURI `json:"uri"`
	Range          Range       `json:"range"`
	SelectionRange Range       `json:"selectionRange"`
	Data           any         `json:"data,omitempty"`
}

// CallHierarchyIncomingCall is one caller plus the ranges of its call sites.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchyIncomingCall
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall is one callee plus the call-site ranges inside the
// document the query started from.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchyOutgoingCall
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyItemParams wraps a resolved item for the follow-up calls.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchyIncomingCallsParams
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchyOutgoingCallsParams
type CallHierarchyItemParams struct {
	Item CallHierarchyItem `json:"item"`
}

// TypeHierarchyItem identifies one node of a type tree.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#typeHierarchyItem
type TypeHierarchyItem struct {
	Name           string      `json:"name"`
	Kind           SymbolKind  `json:"kind"`
	Detail         string      `json:"detail,omitempty"`
	URI            DocumentURI `json:"uri"`
	Range          Range       `json:"range"`
	SelectionRange Range       `json:"selectionRange"`
	Data           any         `json:"data,omitempty"`
}

// TypeHierarchyItemParams wraps a resolved item for supertype/subtype queries.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#typeHierarchySupertypesParams
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#typeHierarchySubtypesParams
type TypeHierarchyItemParams struct {
	Item TypeHierarchyItem `json:"item"`
}

// DidOpenTextDocumentParams carries the full text of a newly opened document.
// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#didOpenTextDocumentParams
type DidOpenTextDocumentParams struct {
	TextDocument TextDocumentItem `json:"textDocument"`
}

// TextDocumentItem is the payload of textDocument/didOpen.
type TextDocumentItem struct {
	URI        DocumentURI `json:"uri"`
	LanguageID string      `json:"languageId"`
	Version    int         `json:"version"`
	Text       string      `json:"text"`
}
