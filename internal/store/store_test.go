package store

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := Open(t.TempDir(), log)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func record(repo, file, name string, kind model.SymbolType) model.SymbolRecord {
	return model.SymbolRecord{
		RepoName:      repo,
		FilePath:      file,
		Language:      model.LanguageC,
		SymbolType:    kind,
		Name:          name,
		StartByte:     12,
		EndByte:       48,
		StartLine:     3,
		EndLine:       9,
		Documentation: "doc for " + name,
		Signature:     string(kind) + " " + name,
		SourceCode:    "body of " + name,
	}
}

func put(t *testing.T, st *Store, rec model.SymbolRecord) string {
	t.Helper()
	key, _, err := st.PutSymbol(rec)
	if err != nil {
		t.Fatalf("put %s: %v", rec.Name, err)
	}
	return key
}

func TestPrimaryKeyLayout(t *testing.T) {
	rec := record("nfs-ganesha", "src/include/fsal_types.h", "fsal_obj_handle", model.SymbolStruct)
	rec.StartByte = 1420

	got := PrimaryKey(rec)
	want := "sym:nfs-ganesha:src/include/fsal_types.h:struct:fsal_obj_handle:00001420"
	if got != want {
		t.Fatalf("PrimaryKey = %q, want %q", got, want)
	}
	// The 8-character zero padding keeps offsets in lexicographic order.
	if offsetToken(7) >= offsetToken(64) || len(offsetToken(7)) != 8 {
		t.Fatalf("offset padding is not order preserving: %q vs %q", offsetToken(7), offsetToken(64))
	}
	if !strings.HasPrefix(nameIndexKey(rec.Name, rec), "idx:name:fsal_obj_handle:nfs-ganesha:") {
		t.Errorf("name index key = %q", nameIndexKey(rec.Name, rec))
	}
	if !strings.HasPrefix(typeIndexKey(rec.Name, rec), "idx:type:struct:fsal_obj_handle:") {
		t.Errorf("type index key = %q", typeIndexKey(rec.Name, rec))
	}
	if fileIndexKey(rec) != "idx:file:nfs-ganesha:src/include/fsal_types.h:00001420" {
		t.Errorf("file index key = %q", fileIndexKey(rec))
	}
}

func TestPutAndLookupExact(t *testing.T) {
	st := testStore(t)
	put(t, st, record("repo-a", "src/a.c", "read_header", model.SymbolFunction))

	got, err := st.Lookup("read_header", Filter{ExactOnly: true})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].SourceCode != "body of read_header" || got[0].Signature != "function read_header" {
		t.Errorf("record round-tripped incorrectly: %+v", got[0])
	}

	empty, err := st.Lookup("absent_symbol", Filter{ExactOnly: true})
	if err != nil {
		t.Fatalf("lookup miss returned an error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("a miss returned %d records", len(empty))
	}
	if _, err := st.Lookup("  ", Filter{}); err == nil {
		t.Error("an empty query must be rejected")
	}
}

func TestPutSkipsUnchangedRecords(t *testing.T) {
	st := testStore(t)
	rec := record("repo-a", "src/a.c", "read_header", model.SymbolFunction)

	if _, written, err := st.PutSymbol(rec); err != nil || !written {
		t.Fatalf("first put: written=%v err=%v", written, err)
	}
	if _, written, err := st.PutSymbol(rec); err != nil || written {
		t.Fatalf("identical put must be a no-op: written=%v err=%v", written, err)
	}

	rec.SourceCode = "body of read_header v2"
	if _, written, err := st.PutSymbol(rec); err != nil || !written {
		t.Fatalf("changed put must write: written=%v err=%v", written, err)
	}
}

func TestPutRejectsInvalidRecords(t *testing.T) {
	st := testStore(t)

	broken := record("repo-a", "src/a.c", "", model.SymbolFunction)
	if _, _, err := st.PutSymbol(broken); err == nil {
		t.Error("a record without a name must be rejected")
	}
	broken = record("repo-a", "src/a.c", "x", model.SymbolType("gadget"))
	if _, _, err := st.PutSymbol(broken); err == nil {
		t.Error("a record with an unknown symbol_type must be rejected")
	}
}

func TestLookupPrefixTypeAndRepoFilters(t *testing.T) {
	st := testStore(t)
	put(t, st, record("repo-a", "src/a.h", "ERR_FSAL_NO_QUOTA", model.SymbolMacro))
	put(t, st, record("repo-a", "src/a.h", "ERR_FSAL_STALE", model.SymbolMacro))
	put(t, st, record("repo-b", "src/b.h", "ERR_FSAL_PERM", model.SymbolMacro))
	put(t, st, record("repo-a", "src/a.h", "ERR_FSAL_HANDLE", model.SymbolStruct))

	all, err := st.Lookup("ERR_FSAL", Filter{})
	if err != nil {
		t.Fatalf("prefix lookup: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("prefix lookup returned %d records, want 4", len(all))
	}

	macros, err := st.Lookup("ERR_FSAL", Filter{Types: []model.SymbolType{model.SymbolMacro}})
	if err != nil {
		t.Fatalf("typed lookup: %v", err)
	}
	if len(macros) != 3 {
		t.Fatalf("typed lookup returned %d records, want 3", len(macros))
	}
	for _, m := range macros {
		if m.SymbolType != model.SymbolMacro {
			t.Errorf("type filter leaked %q", m.SymbolType)
		}
	}

	scoped, err := st.Lookup("ERR_FSAL", Filter{Repo: "repo-b"})
	if err != nil {
		t.Fatalf("scoped lookup: %v", err)
	}
	if len(scoped) != 1 || scoped[0].RepoName != "repo-b" {
		t.Fatalf("repo filter returned %+v", scoped)
	}

	if exact, _ := st.Lookup("ERR_FSAL", Filter{ExactOnly: true}); len(exact) != 0 {
		t.Errorf("ExactOnly matched %d prefix records", len(exact))
	}

	capped, err := st.Lookup("ERR_FSAL", Filter{Limit: 2})
	if err != nil {
		t.Fatalf("capped lookup: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("limit not honoured: got %d", len(capped))
	}
}

func TestLookupResolvesAliasesAndRanksExactFirst(t *testing.T) {
	st := testStore(t)

	method := record("repo-a", "srv.go", "Issue", model.SymbolMethod)
	method.Language = model.LanguageGo
	method.Aliases = []string{"Issuer.Issue"}
	put(t, st, method)
	put(t, st, record("repo-a", "srv.go", "IssueLater", model.SymbolFunction))

	byAlias, err := st.Lookup("Issuer.Issue", Filter{ExactOnly: true})
	if err != nil {
		t.Fatalf("alias lookup: %v", err)
	}
	if len(byAlias) != 1 || byAlias[0].Name != "Issue" {
		t.Fatalf("alias lookup returned %+v", byAlias)
	}

	ranked, err := st.Lookup("Issue", Filter{})
	if err != nil {
		t.Fatalf("ranked lookup: %v", err)
	}
	if len(ranked) != 2 || ranked[0].Name != "Issue" {
		t.Fatalf("exact match was not ranked first: %+v", ranked)
	}
}

func TestPruneRepoInvalidatesStaleRecordsAndIndexes(t *testing.T) {
	st := testStore(t)

	keepRec := record("repo-a", "src/a.c", "still_here", model.SymbolFunction)
	staleRec := record("repo-a", "src/a.c", "removed", model.SymbolFunction)
	staleRec.StartByte, staleRec.EndByte = 900, 940
	staleRec.Aliases = []string{"legacy_removed"}
	otherRepo := record("repo-b", "src/b.c", "untouched", model.SymbolFunction)

	keepKey := put(t, st, keepRec)
	put(t, st, staleRec)
	put(t, st, otherRepo)

	removed, err := st.PruneRepo("repo-a", map[string]struct{}{keepKey: {}})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d records, want 1", removed)
	}

	if got, _ := st.Lookup("still_here", Filter{ExactOnly: true}); len(got) != 1 {
		t.Error("a live record was pruned")
	}
	if got, _ := st.Lookup("removed", Filter{ExactOnly: true}); len(got) != 0 {
		t.Error("a stale record survived the prune")
	}
	if got, _ := st.Lookup("legacy_removed", Filter{ExactOnly: true}); len(got) != 0 {
		t.Error("a stale alias index entry survived the prune")
	}
	if got, _ := st.Lookup("untouched", Filter{ExactOnly: true}); len(got) != 1 {
		t.Error("prune crossed a repository boundary")
	}

	keys, err := st.RepoSymbolKeys("repo-a")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 || keys[0] != keepKey {
		t.Fatalf("RepoSymbolKeys = %v", keys)
	}
}

func TestFileSymbolsAreOrderedBySourcePosition(t *testing.T) {
	st := testStore(t)

	second := record("repo-a", "src/a.c", "second", model.SymbolFunction)
	second.StartByte, second.EndByte = 4000, 4040
	first := record("repo-a", "src/a.c", "first", model.SymbolFunction)
	first.StartByte, first.EndByte = 10, 50
	put(t, st, second)
	put(t, st, first)
	put(t, st, record("repo-a", "src/other.c", "elsewhere", model.SymbolFunction))

	got, err := st.FileSymbols("repo-a", "src/a.c")
	if err != nil {
		t.Fatalf("file symbols: %v", err)
	}
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("FileSymbols = %+v", got)
	}
}

func TestStatsAggregatesPerRepository(t *testing.T) {
	st := testStore(t)

	goFn := record("repo-a", "srv.go", "Serve", model.SymbolFunction)
	goFn.Language = model.LanguageGo
	put(t, st, goFn)
	put(t, st, record("repo-a", "src/a.h", "ROUTER_MAX", model.SymbolMacro))
	put(t, st, record("repo-b", "src/b.c", "handle", model.SymbolFunction))

	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	a, ok := stats["repo-a"]
	if !ok {
		t.Fatal("repo-a missing from stats")
	}
	if a.Symbols != 2 || a.Functions != 1 {
		t.Errorf("repo-a counts = %d symbols / %d functions", a.Symbols, a.Functions)
	}
	if a.SymbolTypes["macro"] != 1 || a.Languages["go"] != 1 || a.Languages["c"] != 1 {
		t.Errorf("repo-a breakdown = %+v / %+v", a.SymbolTypes, a.Languages)
	}
	if len(a.Files) != 2 {
		t.Errorf("repo-a file count = %d, want 2", len(a.Files))
	}
	if stats["repo-b"].Symbols != 1 {
		t.Errorf("repo-b symbol count = %d, want 1", stats["repo-b"].Symbols)
	}
}

func TestValueLogGCIsIdempotent(t *testing.T) {
	st := testStore(t)
	if err := st.RunValueLogGC(0.7); err != nil {
		t.Fatalf("gc on an empty cache must be a no-op: %v", err)
	}
}
