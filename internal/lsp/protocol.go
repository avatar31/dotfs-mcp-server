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
