package utils

import "errors"

var (
	// ErrUnsupportedLanguage marks a file the daemon pool cannot serve.
	ErrUnsupportedLanguage = errors.New("mcp: unsupported language")
)
