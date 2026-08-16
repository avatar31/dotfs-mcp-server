package indexer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
	"github.com/avatar31/dotfs-mcp-server/internal/parser"
	"github.com/avatar31/dotfs-mcp-server/internal/store"
)

func newTestIndexer(t *testing.T, root string) (*Indexer, *store.Store) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir(), log)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	ix, err := New(st, parser.NewDefaultRegistry(), log, Options{
		WorkspaceRoot: root,
		MaxFileSize:   1 << 20,
		SkipDirs:      []string{".git", "vendor"},
		Workers:       2,
	})
	if err != nil {
		t.Fatalf("new indexer: %v", err)
	}
	return ix, st
}

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

func TestIndexAllPopulatesBothLanguages(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, st := newTestIndexer(t, root)

	summaries, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("index all: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("want 2 repository summaries, got %d", len(summaries))
	}

	goRec, err := st.Get("ValidateSessionToken")
	if err != nil {
		t.Fatalf("go record lookup: %v", err)
	}
	if goRec.RepoName != "auth-service-go" {
		t.Fatalf("want repo auth-service-go, got %q", goRec.RepoName)
	}
	if goRec.Language != model.LanguageGo {
		t.Fatalf("want language go, got %q", goRec.Language)
	}
	if goRec.FilePath != "auth-service-go/token.go" {
		t.Fatalf("unexpected file path %q", goRec.FilePath)
	}
	if !strings.Contains(goRec.Documentation, "verifies the HMAC signature") {
		t.Fatalf("documentation was not captured: %q", goRec.Documentation)
	}

	cRec, err := st.Get("read_session_header")
	if err != nil {
		t.Fatalf("c record lookup: %v", err)
	}
	if cRec.RepoName != "packet-router-c" || cRec.Language != model.LanguageC {
		t.Fatalf("unexpected c record: %+v", cRec)
	}
	if !strings.Contains(cRec.SourceCode, "memcpy(out, frame, HEADER_BYTES);") {
		t.Fatalf("c source body was not isolated: %q", cRec.SourceCode)
	}

	// The receiver-qualified alias must also resolve.
	if _, err := st.Get("Issuer.Issue"); err != nil {
		t.Fatalf("method alias lookup: %v", err)
	}
}

func TestIndexRepoPrunesDeletedFunctions(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, st := newTestIndexer(t, root)

	if _, err := ix.IndexRepo(context.Background(), "auth-service-go"); err != nil {
		t.Fatalf("first index: %v", err)
	}

	trimmed := "package auth\n\n// Issuer mints signed session tokens.\ntype Issuer struct{}\n"
	if err := os.WriteFile(filepath.Join(root, "auth-service-go", "token.go"), []byte(trimmed), 0o600); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	summary, err := ix.IndexRepo(context.Background(), "auth-service-go")
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if summary.RecordsPruned == 0 {
		t.Fatal("stale records must be pruned after functions disappear")
	}

	if _, err := st.Get("ValidateSessionToken"); err == nil {
		t.Fatal("deleted function must no longer resolve")
	}
}

func TestSearchLiveFindsUncachedFunction(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, _ := newTestIndexer(t, root)

	rec, found, err := ix.SearchLive(context.Background(), "route_packet")
	if err != nil {
		t.Fatalf("live scan: %v", err)
	}
	if !found {
		t.Fatal("route_packet must be discovered by the live fallback scan")
	}
	if rec.RepoName != "packet-router-c" || rec.Language != model.LanguageC {
		t.Fatalf("unexpected live scan record: %+v", rec)
	}

	if _, _, err := ix.SearchLive(context.Background(), "definitely_not_here"); err != nil {
		t.Fatalf("missing symbol must not error: %v", err)
	}
}

func TestValidateRepoNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../etc", "auth/../..", `windows\path`, "sub/dir", "-leading"} {
		if err := ValidateRepoName(name); err == nil {
			t.Fatalf("repository name %q must be rejected", name)
		}
	}
	for _, name := range []string{"auth-service-go", "packet_router.c1", "svc123"} {
		if err := ValidateRepoName(name); err != nil {
			t.Fatalf("repository name %q must be accepted: %v", name, err)
		}
	}
}

func TestSafeRepoPathRejectsEscape(t *testing.T) {
	root := fixtureWorkspace(t)

	if _, err := SafeRepoPath(root, "../"); err == nil {
		t.Fatal("parent traversal must be rejected")
	}
	if _, err := SafeRepoPath(root, "missing-service"); err == nil {
		t.Fatal("unknown repository must be rejected")
	}

	got, err := SafeRepoPath(root, "auth-service-go")
	if err != nil {
		t.Fatalf("valid repository rejected: %v", err)
	}
	if filepath.Base(got) != "auth-service-go" {
		t.Fatalf("unexpected resolved path %q", got)
	}
}

func TestListReposIgnoresSkippedDirectories(t *testing.T) {
	root := fixtureWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, "vendor"), 0o755); err != nil {
		t.Fatalf("create vendor dir: %v", err)
	}

	ix, _ := newTestIndexer(t, root)
	repos, err := ix.ListRepos()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "auth-service-go" || repos[1] != "packet-router-c" {
		t.Fatalf("unexpected repository list: %v", repos)
	}
}
