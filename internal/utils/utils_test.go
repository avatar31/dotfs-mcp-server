package utils

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// fixtureWorkspace copies the checked-in sample workspace into a temp dir so
// tests can mutate it freely.
func fixtureWorkspace(t *testing.T) string {
	t.Helper()

	src := filepath.Join("..", "..", "testdata", "workspace")
	dst := t.TempDir()

	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copy fixture workspace: %v", err)
	}
	return dst
}

func TestValidateRepoName(t *testing.T) {
	for _, bad := range []string{"", " ", ".", "..", "../etc", "a/b", `a\b`, "with space", "-leading"} {
		if err := ValidateRepoName(bad); err == nil {
			t.Errorf("ValidateRepoName(%q) must fail", bad)
		}
	}
	for _, good := range []string{"nfs-ganesha", "auth-service-go", "repo_1", "Repo.v2"} {
		if err := ValidateRepoName(good); err != nil {
			t.Errorf("ValidateRepoName(%q) = %v", good, err)
		}
	}
}

func TestSafeRepoPath(t *testing.T) {
	root := fixtureWorkspace(t)
	if _, err := SafeRepoPath(root, "packet-router-c"); err != nil {
		t.Errorf("SafeRepoPath rejected a valid repository: %v", err)
	}
	if _, err := SafeRepoPath(root, "does-not-exist"); err == nil {
		t.Error("SafeRepoPath accepted a missing repository")
	}
}

func TestLanguageFor(t *testing.T) {
	cases := map[string]model.Language{
		"token.go":   model.LanguageGo,
		"router.c":   model.LanguageC,
		"router.h":   model.LanguageC,
		"engine.cpp": model.LanguageC,
		"engine.hpp": model.LanguageC,
	}
	for path, want := range cases {
		got, err := LanguageFor(path)
		if err != nil || got != want {
			t.Errorf("LanguageFor(%q) = %q, %v", path, got, err)
		}
	}
	if _, err := LanguageFor("notes.md"); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Errorf("LanguageFor(md) = %v", err)
	}
}

func TestLanguageID(t *testing.T) {
	if got := LanguageID(model.LanguageGo, "token.go"); got != "go" {
		t.Errorf("go languageId = %q", got)
	}
	if got := LanguageID(model.LanguageC, "router.c"); got != "c" {
		t.Errorf("c languageId = %q", got)
	}
	if got := LanguageID(model.LanguageC, "engine.cpp"); got != "cpp" {
		t.Errorf("cpp languageId = %q", got)
	}
}
