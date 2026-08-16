package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/avatar31/dotfs-mcp-server/internal/capabilities"
	"github.com/avatar31/dotfs-mcp-server/internal/model"
	"github.com/avatar31/dotfs-mcp-server/internal/store"
)

type stubCache struct {
	records map[string]model.FunctionRecord
	stats   map[string]store.RepoStat
}

func (s stubCache) Get(name string) (model.FunctionRecord, error) {
	rec, ok := s.records[name]
	if !ok {
		return model.FunctionRecord{}, store.ErrNotFound
	}
	return rec, nil
}

func (s stubCache) Stats() (map[string]store.RepoStat, error) { return s.stats, nil }

type stubScanner struct {
	record model.FunctionRecord
	found  bool
	err    error
	calls  int
}

func (s *stubScanner) SearchLive(context.Context, string) (model.FunctionRecord, bool, error) {
	s.calls++
	return s.record, s.found, s.err
}

func (s *stubScanner) ListRepos() ([]string, error) { return []string{"auth-service-go"}, nil }

func newDeps(cache Cache, scanner LiveScanner, profiles []capabilities.Profile) Deps {
	return Deps{
		Cache:    cache,
		Scanner:  scanner,
		Matrix:   capabilities.NewMatrix(profiles),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Name:     "test",
		Version:  "0.0.1",
		LiveScan: true,
	}
}

func callTool(t *testing.T, args map[string]any) mcp.CallToolRequest {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("tool returned no content")
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", res.Content[0])
	}
	return text.Text
}

func TestGlobalSearchReturnsCachedRecordAsJSON(t *testing.T) {
	rec := model.FunctionRecord{
		RepoName:      "packet-router-c",
		FilePath:      "packet-router-c/router.c",
		Language:      model.LanguageC,
		Documentation: "read_session_header copies the header.",
		SourceCode:    "int read_session_header(void) { return 0; }",
	}
	scanner := &stubScanner{}
	deps := newDeps(stubCache{records: map[string]model.FunctionRecord{"read_session_header": rec}}, scanner, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "read_session_header",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}

	var got model.FunctionRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("tool output is not valid JSON: %v", err)
	}
	if got != rec {
		t.Fatalf("payload mismatch:\n got %+v\nwant %+v", got, rec)
	}
	if scanner.calls != 0 {
		t.Fatal("a cache hit must not trigger the live fallback scan")
	}
}

func TestGlobalSearchFallsBackToLiveScanOnMiss(t *testing.T) {
	rec := model.FunctionRecord{
		RepoName:   "auth-service-go",
		FilePath:   "auth-service-go/token.go",
		Language:   model.LanguageGo,
		SourceCode: "func ValidateSessionToken() {}",
	}
	scanner := &stubScanner{record: rec, found: true}
	deps := newDeps(stubCache{}, scanner, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "ValidateSessionToken",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if scanner.calls != 1 {
		t.Fatalf("want exactly one live scan, got %d", scanner.calls)
	}
	if !strings.Contains(resultText(t, res), `"repo_name": "auth-service-go"`) {
		t.Fatalf("unexpected payload: %s", resultText(t, res))
	}
}

func TestGlobalSearchReportsUnknownSymbol(t *testing.T) {
	deps := newDeps(stubCache{}, &stubScanner{}, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "nope",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("an unknown symbol must be reported as a tool error")
	}
}

func TestGlobalSearchSurfacesScannerFailures(t *testing.T) {
	deps := newDeps(stubCache{}, &stubScanner{err: errors.New("disk on fire")}, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "boom",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("scanner failures must be reported as a tool error")
	}
}

func TestGlobalSearchValidatesInput(t *testing.T) {
	deps := newDeps(stubCache{}, &stubScanner{}, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "   ",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("blank symbols must be rejected")
	}
}

func TestListRepoCapabilitiesMergesProfileAndCacheFacts(t *testing.T) {
	cache := stubCache{stats: map[string]store.RepoStat{
		"auth-service-go": {
			RepoName:  "auth-service-go",
			Functions: 3,
			Languages: map[string]int{"go": 3},
			Files:     map[string]int{"auth-service-go/token.go": 3},
			Samples:   []string{"ValidateSessionToken"},
		},
	}}
	deps := newDeps(cache, &stubScanner{}, []capabilities.Profile{{
		Repo:     "auth-service-go",
		Language: "Go 1.22",
		Summary:  "Owns session token issuance.",
		Features: []string{"HMAC verification"},
	}})

	res, err := deps.handleListCapabilities(context.Background(), callTool(t, map[string]any{
		"repo_name": "auth-service-go",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}

	text := resultText(t, res)
	for _, want := range []string{"Go 1.22", "Owns session token issuance.", "HMAC verification", "Cached functions: 3", "ValidateSessionToken"} {
		if !strings.Contains(text, want) {
			t.Fatalf("briefing is missing %q:\n%s", want, text)
		}
	}
}

func TestListRepoCapabilitiesRejectsUnsafeName(t *testing.T) {
	deps := newDeps(stubCache{}, &stubScanner{}, nil)

	res, err := deps.handleListCapabilities(context.Background(), callTool(t, map[string]any{
		"repo_name": "../../etc",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatal("path traversal in repo_name must be rejected")
	}
}
