package store

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	st, err := Open(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func sampleRecord() model.FunctionRecord {
	return model.FunctionRecord{
		RepoName:      "auth-service-go",
		FilePath:      "auth-service-go/token.go",
		Language:      model.LanguageGo,
		Documentation: "ValidateSessionToken verifies the HMAC signature.",
		SourceCode:    "func ValidateSessionToken(raw []byte) (string, error) { return \"\", nil }",
	}
}

func TestPutAndGetRoundTrip(t *testing.T) {
	st := newTestStore(t)
	rec := sampleRecord()

	written, err := st.Put("ValidateSessionToken", rec)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !written {
		t.Fatal("first write must report a structural delta")
	}

	got, err := st.Get("ValidateSessionToken")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != rec {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, rec)
	}
}

func TestPutSkipsWriteWithoutStructuralDelta(t *testing.T) {
	st := newTestStore(t)
	rec := sampleRecord()

	if _, err := st.Put("ValidateSessionToken", rec); err != nil {
		t.Fatalf("first put: %v", err)
	}

	written, err := st.Put("ValidateSessionToken", rec)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if written {
		t.Fatal("an identical record must not trigger a physical write")
	}

	rec.SourceCode += "\n// changed"
	written, err = st.Put("ValidateSessionToken", rec)
	if err != nil {
		t.Fatalf("third put: %v", err)
	}
	if !written {
		t.Fatal("a changed record must trigger a write")
	}
}

func TestPutRejectsInvalidRecords(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Put("", sampleRecord()); err == nil {
		t.Fatal("empty function names must be rejected")
	}

	bad := sampleRecord()
	bad.Language = "rust"
	if _, err := st.Put("Whatever", bad); err == nil {
		t.Fatal("unsupported language tags must be rejected")
	}
}

func TestGetMissingKeyReturnsErrNotFound(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Get("does_not_exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPruneRepoRemovesOnlyOwnedRecords(t *testing.T) {
	st := newTestStore(t)

	goRec := sampleRecord()
	if _, err := st.Put("ValidateSessionToken", goRec); err != nil {
		t.Fatalf("put go record: %v", err)
	}

	cRec := model.FunctionRecord{
		RepoName:      "packet-router-c",
		FilePath:      "packet-router-c/router.c",
		Language:      model.LanguageC,
		Documentation: "read_session_header copies the header.",
		SourceCode:    "int read_session_header(void) { return 0; }",
	}
	if _, err := st.Put("read_session_header", cRec); err != nil {
		t.Fatalf("put c record: %v", err)
	}

	names, err := st.ListRepoFunctions("auth-service-go")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0] != "ValidateSessionToken" {
		t.Fatalf("unexpected repo index: %v", names)
	}

	removed, err := st.PruneRepo("auth-service-go", []string{"ValidateSessionToken", "read_session_header"})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("want 1 removal, got %d", removed)
	}

	if _, err := st.Get("ValidateSessionToken"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owned record must be evicted, got %v", err)
	}
	if _, err := st.Get("read_session_header"); err != nil {
		t.Fatalf("a record owned by another repository must survive: %v", err)
	}
}

func TestStatsAggregatesPerRepository(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Put("ValidateSessionToken", sampleRecord()); err != nil {
		t.Fatalf("put: %v", err)
	}

	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	stat, ok := stats["auth-service-go"]
	if !ok {
		t.Fatalf("missing repository in stats: %+v", stats)
	}
	if stat.Functions != 1 || stat.Languages["go"] != 1 || len(stat.Files) != 1 {
		t.Fatalf("unexpected stats: %+v", stat)
	}
}
