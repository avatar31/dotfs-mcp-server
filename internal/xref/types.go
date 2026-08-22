// Package xref turns raw Language Server Protocol answers into the compact,
// token-bounded payloads handed to the LLM.
//
// A single textDocument/references call on a widely used symbol can return
// hundreds of URI/range pairs worth thousands of tokens. Everything in this
// package exists to enforce the guardrails: deduplicate, cap the result set,
// resolve each hit back to a repository-relative path and attach exactly one
// line of source as evidence.
package xref

const (
	// MaxResults is the hard ceiling on any relational answer.
	MaxResults = 20

	// maxSnippetChars keeps a single evidence line from blowing the token budget on
	// generated or minified sources.
	maxSnippetChars = 240

	// Call hierarchy directions.
	DirectionIncoming = "incoming"
	DirectionOutgoing = "outgoing"

	// Type hierarchy directions.
	DirectionSupertypes = "supertypes"
	DirectionSubtypes   = "subtypes"
	DirectionBoth       = "both"
)

// Position identifies a symbol occurrence. Line and Character are one-based, as
// specified by the tool contract, and converted to the protocol's zero-based
// coordinates at the boundary.
type Position struct {
	Repo      string
	FilePath  string
	Line      int
	Character int
}

// ReferenceRequest asks for every usage of the symbol under Position.
type ReferenceRequest struct {
	Position
	IncludeDeclaration bool
}

// CallHierarchyRequest asks for callers or callees of the enclosing function.
type CallHierarchyRequest struct {
	Position
	// Direction is "incoming" (callers) or "outgoing" (callees).
	Direction string
}

// TypeHierarchyRequest asks for the definition and relatives of a type.
type TypeHierarchyRequest struct {
	Position
	// Direction is "supertypes", "subtypes" or "both".
	Direction string
}

// Reference is one compacted hit: where it is and what the line says.
//
// Repo is the workspace repository name, or "(external)" for a result outside
// the workspace such as the Go standard library or the module cache; those
// paths are elided to their last segments so absolute host paths never reach
// the model.
type Reference struct {
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Repo      string `json:"repo"`
	FilePath  string `json:"file_path"`
	Line      int    `json:"line"`
	Character int    `json:"character,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
}

// ReferenceResult is the payload of find_references.
type ReferenceResult struct {
	Symbol          string      `json:"symbol"`
	Repo            string      `json:"repo"`
	FilePath        string      `json:"file_path"`
	TotalReferences int         `json:"total_references"`
	Truncated       bool        `json:"truncated,omitempty"`
	References      []Reference `json:"references"`
	Hint            string      `json:"hint,omitempty"`
}

// Call is one edge of a call tree.
type Call struct {
	CallerName string `json:"caller_name,omitempty"`
	CalleeName string `json:"callee_name,omitempty"`
	Repo       string `json:"repo"`
	FilePath   string `json:"file_path"`
	Line       int    `json:"line"`
	Snippet    string `json:"snippet,omitempty"`
}

// CallHierarchyResult is the payload of get_call_hierarchy.
type CallHierarchyResult struct {
	Symbol       string `json:"symbol"`
	Direction    string `json:"direction"`
	TotalCallers int    `json:"total_callers,omitempty"`
	TotalCallees int    `json:"total_callees,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	Calls        []Call `json:"calls"`
	Hint         string `json:"hint,omitempty"`
}

// ImplementationResult is the payload of find_interface_implementations.
type ImplementationResult struct {
	Symbol               string      `json:"symbol"`
	TotalImplementations int         `json:"total_implementations"`
	Truncated            bool        `json:"truncated,omitempty"`
	Implementations      []Reference `json:"implementations"`
	Hint                 string      `json:"hint,omitempty"`
}

// TypeHierarchyResult is the payload of get_type_hierarchy.
type TypeHierarchyResult struct {
	Symbol          string      `json:"symbol"`
	Definition      *Reference  `json:"definition,omitempty"`
	TotalSupertypes int         `json:"total_supertypes"`
	Supertypes      []Reference `json:"supertypes,omitempty"`
	TotalSubtypes   int         `json:"total_subtypes"`
	Subtypes        []Reference `json:"subtypes,omitempty"`
	Hint            string      `json:"hint,omitempty"`
}

// fallbackHint is attached to every empty answer so the agent degrades to the
// Phase 2 index instead of concluding the symbol does not exist.
const fallbackHint = "The language server resolved no result at this position. " +
	"Verify the line/character point at the identifier itself, then fall back to " +
	"lookup_symbol or global_codebase_search for the static declaration."
