package utils

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
