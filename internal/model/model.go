// Package model defines the wire and storage schema shared by the parser,
// cache and MCP tool layers.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// FunctionRecord is the JSON value stored under the "func:<function_name>" key
// in BadgerDB. The field set and JSON names are contractual: the LLM client
// consumes this document verbatim.
type FunctionRecord struct {
	RepoName      string   `json:"repo_name"`
	FilePath      string   `json:"file_path"`
	Language      Language `json:"language"`
	Documentation string   `json:"documentation"`
	SourceCode    string   `json:"source_code"`
}

// Validate guards against writing structurally incomplete records into cache.
func (r FunctionRecord) Validate() error {
	if strings.TrimSpace(r.RepoName) == "" {
		return fmt.Errorf("record is missing repo_name")
	}
	if strings.TrimSpace(r.FilePath) == "" {
		return fmt.Errorf("record is missing file_path")
	}
	if !r.Language.Valid() {
		return fmt.Errorf("record has unsupported language %q, want %q or %q", r.Language, LanguageC, LanguageGo)
	}
	if strings.TrimSpace(r.SourceCode) == "" {
		return fmt.Errorf("record is missing source_code")
	}
	return nil
}

// Fingerprint returns a stable digest of the record used to detect a structural
// delta before issuing a BadgerDB write.
func (r FunctionRecord) Fingerprint() string {
	h := sha256.New()
	for _, part := range []string{r.RepoName, r.FilePath, string(r.Language), r.Documentation, r.SourceCode} {
		// The length prefix makes the concatenation unambiguous.
		fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
