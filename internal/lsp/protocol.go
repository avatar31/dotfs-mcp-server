// Package lsp implements the subset of the Language Server Protocol required to
// drive clangd and gopls as supervised child processes: JSON-RPC 2.0 framing, a
// request/response client and a lazily initialised, per-repository daemon pool.
//
// Only the messages Phase 3 actually sends are modelled. Everything else the
// server pushes at us (progress, diagnostics, log messages) is answered
// generically so a daemon can never block waiting on the orchestrator.
package lsp

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
