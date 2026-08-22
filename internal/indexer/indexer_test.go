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

// lookupOne resolves exactly one cached record by exact name.
func lookupOne(t *testing.T, st *store.Store, name string, types ...model.SymbolType) model.SymbolRecord {
	t.Helper()

	got, err := st.Lookup(name, store.Filter{ExactOnly: true, Types: types})
	if err != nil {
		t.Fatalf("lookup %s: %v", name, err)
	}
	if len(got) == 0 {
		t.Fatalf("symbol %q was not indexed", name)
	}
	return got[0]
}

func TestIndexAllPopulatesEveryConstructOfBothLanguages(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, st := newTestIndexer(t, root)

	summaries, err := ix.IndexAll(context.Background())
	if err != nil {
		t.Fatalf("index all: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("indexed %d repositories, want 2", len(summaries))
	}
	for _, s := range summaries {
		if s.SymbolsFound == 0 || s.RecordsWritten == 0 {
			t.Errorf("repository %s produced no symbols: %+v", s.Repo, s)
		}
	}

	// Go constructs.
	fn := lookupOne(t, st, "ValidateSessionToken")
	if fn.RepoName != "auth-service-go" || fn.Language != model.LanguageGo {
		t.Errorf("unexpected provenance: %+v", fn)
	}
	if fn.FilePath != "token.go" {
		t.Errorf("file_path must be repository-relative, got %q", fn.FilePath)
	}
	if fn.SymbolType != model.SymbolFunction || fn.StartLine == 0 {
		t.Errorf("unexpected function record: %+v", fn)
	}
	if st := lookupOne(t, st, "Session", model.SymbolStruct); !strings.Contains(st.SourceCode, "TTL") {
		t.Errorf("struct body was not cached: %q", st.SourceCode)
	}
	if iface := lookupOne(t, st, "SessionStore", model.SymbolInterface); iface.SymbolType != model.SymbolInterface {
		t.Errorf("interface record = %+v", iface)
	}
	if c := lookupOne(t, st, "StatusPending", model.SymbolConstant); !strings.Contains(c.SourceCode, "iota") {
		t.Errorf("const block was not cached: %q", c.SourceCode)
	}
	if alias := lookupOne(t, st, "AccountID", model.SymbolTypeAlias); alias.Signature != "type AccountID = string" {
		t.Errorf("type alias signature = %q", alias.Signature)
	}

	// C constructs.
	macro := lookupOne(t, st, "ROUTER_QUEUE_DEPTH", model.SymbolMacro)
	if macro.RepoName != "packet-router-c" || macro.Language != model.LanguageC {
		t.Errorf("unexpected macro provenance: %+v", macro)
	}
	if fnMacro := lookupOne(t, st, "ROUTER_MIN", model.SymbolMacroFunction); fnMacro.Signature != "#define ROUTER_MIN(a, b)" {
		t.Errorf("macro function signature = %q", fnMacro.Signature)
	}
	if rec := lookupOne(t, st, "router_ops", model.SymbolStruct); !strings.Contains(rec.SourceCode, "(*open)") {
		t.Errorf("v-table body was not cached: %q", rec.SourceCode)
	}
	if td := lookupOne(t, st, "router_status_t", model.SymbolTypedef); td.SymbolType != model.SymbolTypedef {
		t.Errorf("typedef record = %+v", td)
	}
	if e := lookupOne(t, st, "router_priority", model.SymbolEnum); e.SymbolType != model.SymbolEnum {
		t.Errorf("enum record = %+v", e)
	}
	if quota := lookupOne(t, st, "ROUTER_ERR_NO_QUOTA", model.SymbolConstant); quota.ParentScope != "router_status_t" {
		t.Errorf("enumerator scope = %q", quota.ParentScope)
	}

	// The Go method must be addressable under both its plain and qualified name.
	plain := lookupOne(t, st, "Issue")
	qualified := lookupOne(t, st, "Issuer.Issue")
	if plain.SourceCode != qualified.SourceCode || qualified.SymbolType != model.SymbolMethod {
		t.Errorf("method alias did not resolve to the same record: %+v vs %+v", plain, qualified)
	}
}

func TestIndexRepoIsIncrementalAndInvalidatesRemovedSymbols(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, st := newTestIndexer(t, root)

	first, err := ix.IndexRepo(context.Background(), "packet-router-c")
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	if first.RecordsWritten == 0 {
		t.Fatal("the cold index wrote nothing")
	}

	// An unchanged tree must not trigger a single physical write.
	second, err := ix.IndexRepo(context.Background(), "packet-router-c")
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if second.RecordsWritten != 0 {
		t.Errorf("re-indexing an unchanged repo wrote %d records", second.RecordsWritten)
	}
	if second.RecordsPruned != 0 {
		t.Errorf("re-indexing an unchanged repo pruned %d records", second.RecordsPruned)
	}

	// Deleting the header must evict every symbol it contributed.
	if err := os.Remove(filepath.Join(root, "packet-router-c", "router.h")); err != nil {
		t.Fatalf("remove header: %v", err)
	}
	third, err := ix.IndexRepo(context.Background(), "packet-router-c")
	if err != nil {
		t.Fatalf("third index: %v", err)
	}
	if third.RecordsPruned == 0 {
		t.Error("deleting a header pruned nothing")
	}
	for _, gone := range []string{"ROUTER_QUEUE_DEPTH", "router_ops", "router_status_t", "ROUTER_PRIORITY_LOW"} {
		if got, _ := st.Lookup(gone, store.Filter{ExactOnly: true}); len(got) != 0 {
			t.Errorf("symbol %q survived invalidation", gone)
		}
	}
	// Symbols still present in router.c must be untouched.
	if got, _ := st.Lookup("read_session_header", store.Filter{ExactOnly: true}); len(got) != 1 {
		t.Error("invalidation removed a live symbol")
	}
}

func TestSearchLiveBackfillsTheCache(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, st := newTestIndexer(t, root)

	if got, _ := st.Lookup("read_session_header", store.Filter{ExactOnly: true}); len(got) != 0 {
		t.Fatal("the cache should start empty")
	}

	matches, err := ix.SearchLive(context.Background(), "read_session_header", model.CallableTypes)
	if err != nil {
		t.Fatalf("live scan: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("live scan returned %d matches, want 1", len(matches))
	}
	if matches[0].RepoName != "packet-router-c" || matches[0].FilePath != "router.c" {
		t.Errorf("unexpected live match: %+v", matches[0])
	}

	if got, _ := st.Lookup("read_session_header", store.Filter{ExactOnly: true}); len(got) != 1 {
		t.Error("the live scan did not back-fill the cache")
	}

	// A type filter must exclude a callable match.
	none, err := ix.SearchLive(context.Background(), "read_session_header", []model.SymbolType{model.SymbolStruct})
	if err != nil {
		t.Fatalf("filtered live scan: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("type filter leaked %d matches", len(none))
	}
}

func TestReadSnippetIsBoundedAndContained(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, _ := newTestIndexer(t, root)

	snippet, err := ix.ReadSnippet("packet-router-c", "router.c", 1, 4)
	if err != nil {
		t.Fatalf("read snippet: %v", err)
	}
	if !strings.Contains(snippet, "packet-router-c/router.c:1-4") {
		t.Errorf("snippet header missing: %q", snippet)
	}
	if !strings.Contains(snippet, "#define HEADER_BYTES 32") {
		t.Errorf("snippet body missing: %q", snippet)
	}
	if lines := strings.Count(snippet, "\n"); lines != 5 {
		t.Errorf("snippet has %d newlines, want 5 (header + 4 lines)", lines)
	}

	// The range is clamped rather than rejected.
	long, err := ix.ReadSnippet("packet-router-c", "router.c", 1, 10_000)
	if err != nil {
		t.Fatalf("clamped snippet: %v", err)
	}
	if strings.Count(long, "\n") > MaxSnippetLines+1 {
		t.Errorf("snippet exceeded the %d line cap", MaxSnippetLines)
	}

	for _, bad := range []struct{ repo, path string }{
		{"packet-router-c", "../auth-service-go/token.go"},
		{"packet-router-c", "/etc/passwd"},
		{"../packet-router-c", "router.c"},
		{"packet-router-c", "missing.c"},
	} {
		if _, err := ix.ReadSnippet(bad.repo, bad.path, 1, 5); err == nil {
			t.Errorf("ReadSnippet(%q, %q) must fail", bad.repo, bad.path)
		}
	}
	if _, err := ix.ReadSnippet("packet-router-c", "router.c", 9, 3); err == nil {
		t.Error("an inverted line range must be rejected")
	}
}

func TestListReposSkipsNonAddressableEntries(t *testing.T) {
	root := fixtureWorkspace(t)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatalf("write loose file: %v", err)
	}

	ix, _ := newTestIndexer(t, root)
	repos, err := ix.ListRepos()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "auth-service-go" || repos[1] != "packet-router-c" {
		t.Fatalf("ListRepos = %v", repos)
	}
}

func TestIndexRepoHonoursContextCancellation(t *testing.T) {
	root := fixtureWorkspace(t)
	ix, _ := newTestIndexer(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := ix.IndexRepo(ctx, "packet-router-c"); err == nil {
		t.Error("IndexRepo ignored a cancelled context")
	}
}
