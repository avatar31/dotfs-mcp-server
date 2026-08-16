// Package model defines the wire and storage schema shared by the parser,
// cache and MCP tool layers.
package model

// Language classifies the runtime environment a symbol belongs to. The MCP
// client uses it to pick syntax-specific markdown fences and linting rules.
type Language string

// Supported language classifications.
const (
	LanguageC  Language = "c"
	LanguageGo Language = "go"
)
