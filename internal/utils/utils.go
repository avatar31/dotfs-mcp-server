package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// repoNamePattern deliberately excludes path separators, whitespace and dots-only
// names so a repository identifier can never escape the workspace root.
var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateRepoName rejects any identifier that could be used for path traversal.
func ValidateRepoName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("repository name must not be empty")
	case name == "." || name == "..":
		return fmt.Errorf("repository name %q is not addressable", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("repository name %q must not contain a path separator", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("repository name %q must not contain a parent directory reference", name)
	case !repoNamePattern.MatchString(name):
		return fmt.Errorf("repository name %q must match %s", name, repoNamePattern)
	}
	return nil
}

// SafeRepoPath resolves <root>/<name> and proves the result is a directory that
// still lives inside root after symlink evaluation.
func SafeRepoPath(root, name string) (string, error) {
	if err := ValidateRepoName(name); err != nil {
		return "", err
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root %q: %w", root, err)
	}

	candidate := filepath.Join(realRoot, name)
	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve repository %q: %w", name, err)
	}
	if !IsWithinRepo(realRoot, realPath) {
		return "", fmt.Errorf("repository %q resolves outside the workspace root", name)
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return "", fmt.Errorf("stat repository %q: %w", name, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository %q is not a directory", name)
	}
	return realPath, nil
}

// IsWithinRepo reports whether child is root itself or nested under it.
func IsWithinRepo(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// LanguageFor maps a file extension onto the language server that owns it.
func LanguageFor(path string) (model.Language, error) {
	switch filepath.Ext(path) {
	case ".go":
		return model.LanguageGo, nil
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx":
		return model.LanguageC, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedLanguage, filepath.Ext(path))
	}
}

// LanguageID renders the LSP languageId for a document.
func LanguageID(lang model.Language, path string) string {
	if lang == model.LanguageGo {
		return "go"
	}
	switch filepath.Ext(path) {
	case ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx":
		return "cpp"
	default:
		return "c"
	}
}
