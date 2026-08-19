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
)
