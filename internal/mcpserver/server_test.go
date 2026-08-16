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

// stubCache is an in-memory Cache honouring the prefix, type and repo filters.
type stubCache struct {
	records []model.SymbolRecord
	stats   map[string]store.RepoStat
	err     error
	calls   int
}

func (s *stubCache) Lookup(name string, f store.Filter) ([]model.SymbolRecord, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	allowed := make(map[model.SymbolType]struct{}, len(f.Types))
	for _, t := range f.Types {
		allowed[t] = struct{}{}
	}

	var out []model.SymbolRecord
	for _, rec := range s.records {
		if f.ExactOnly && rec.Name != name && !slicesContains(rec.Aliases, name) {
			continue
		}
		if !f.ExactOnly && !strings.HasPrefix(rec.Name, name) {
			continue
		}
		if f.Repo != "" && rec.RepoName != f.Repo {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[rec.SymbolType]; !ok {
				continue
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

func (s *stubCache) Stats() (map[string]store.RepoStat, error) { return s.stats, nil }

func slicesContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

type stubScanner struct {
	records []model.SymbolRecord
	err     error
	calls   int
	snippet string
	snipErr error
	gotArgs []any
}

func (s *stubScanner) SearchLive(_ context.Context, target string, types []model.SymbolType) ([]model.SymbolRecord, error) {
	s.calls++
	s.gotArgs = []any{target, types}
	return s.records, s.err
}

func (s *stubScanner) ListRepos() ([]string, error) { return []string{"auth-service-go"}, nil }

func (s *stubScanner) ReadSnippet(repo, path string, start, end int) (string, error) {
	s.gotArgs = []any{repo, path, start, end}
	return s.snippet, s.snipErr
}

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

func sym(repo, name string, kind model.SymbolType, lang model.Language) model.SymbolRecord {
	return model.SymbolRecord{
		RepoName:      repo,
		FilePath:      "src/main" + map[model.Language]string{model.LanguageC: ".c", model.LanguageGo: ".go"}[lang],
		Language:      lang,
		SymbolType:    kind,
		Name:          name,
		StartByte:     10,
		EndByte:       90,
		StartLine:     4,
		EndLine:       12,
		Documentation: name + " documentation",
		Signature:     string(kind) + " " + name,
		SourceCode:    "source of " + name,
	}
}

func TestNewRegistersTheFullToolkit(t *testing.T) {
	srv, err := New(newDeps(&stubCache{}, &stubScanner{}, nil))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if srv == nil {
		t.Fatal("New returned a nil server")
	}
	if _, err := New(Deps{}); err == nil {
		t.Error("New accepted incomplete dependencies")
	}
}

func TestGlobalSearchReturnsCachedRecordAsJSON(t *testing.T) {
	rec := sym("packet-router-c", "read_session_header", model.SymbolFunction, model.LanguageC)
	scanner := &stubScanner{}
	deps := newDeps(&stubCache{records: []model.SymbolRecord{rec}}, scanner, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "read_session_header",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if scanner.calls != 0 {
		t.Error("a cache hit must not trigger a live scan")
	}

	var got model.SymbolRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if got.Name != rec.Name || got.SymbolType != model.SymbolFunction || got.StartLine != 4 {
		t.Errorf("payload lost fields: %+v", got)
	}
}

func TestGlobalSearchFallsBackToLiveScan(t *testing.T) {
	live := sym("packet-router-c", "route_packet", model.SymbolFunction, model.LanguageC)
	scanner := &stubScanner{records: []model.SymbolRecord{live}}
	deps := newDeps(&stubCache{}, scanner, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "route_packet",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if scanner.calls != 1 {
		t.Errorf("live scan ran %d times, want 1", scanner.calls)
	}
	if types, ok := scanner.gotArgs[1].([]model.SymbolType); !ok || len(types) != 2 {
		t.Errorf("global search must restrict the live scan to callables, got %v", scanner.gotArgs[1])
	}
	if !strings.Contains(resultText(t, res), "route_packet") {
		t.Errorf("live record was not rendered: %s", resultText(t, res))
	}
}

func TestGlobalSearchReportsMissesAndBadInput(t *testing.T) {
	deps := newDeps(&stubCache{}, &stubScanner{}, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "nope",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "No indexed function") {
		t.Errorf("a miss must be reported as a tool error: %s", resultText(t, res))
	}

	for _, args := range []map[string]any{{}, {"target_function_name": "   "}, {"target_function_name": 42}} {
		res, err := deps.handleGlobalSearch(context.Background(), callTool(t, args))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError {
			t.Errorf("invalid arguments %v were accepted", args)
		}
	}
}

func TestGlobalSearchSurfacesCacheFailures(t *testing.T) {
	deps := newDeps(&stubCache{err: errors.New("badger is on fire")}, &stubScanner{}, nil)

	res, err := deps.handleGlobalSearch(context.Background(), callTool(t, map[string]any{
		"target_function_name": "anything",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "badger is on fire") {
		t.Errorf("cache failure was not surfaced: %s", resultText(t, res))
	}
}

func TestLookupSymbolSupportsPrefixAndFilters(t *testing.T) {
	cache := &stubCache{records: []model.SymbolRecord{
		sym("nfs-ganesha", "ERR_FSAL_NO_QUOTA", model.SymbolMacro, model.LanguageC),
		sym("nfs-ganesha", "ERR_FSAL_STALE", model.SymbolMacro, model.LanguageC),
		sym("auth-service-go", "ERR_FSAL_HANDLE", model.SymbolStruct, model.LanguageGo),
	}}
	deps := newDeps(cache, &stubScanner{}, nil)

	res, err := deps.handleLookupSymbol(context.Background(), callTool(t, map[string]any{"name": "ERR_FSAL"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var all []model.SymbolRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &all); err != nil {
		t.Fatalf("payload is not a JSON array: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("prefix lookup returned %d records, want 3", len(all))
	}

	res, err = deps.handleLookupSymbol(context.Background(), callTool(t, map[string]any{
		"name":        "ERR_FSAL",
		"symbol_type": "macro",
		"repo_name":   "nfs-ganesha",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var filtered []model.SymbolRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &filtered); err != nil {
		t.Fatalf("payload is not a JSON array: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered lookup returned %d records, want 2", len(filtered))
	}

	for _, args := range []map[string]any{
		{"name": "X", "symbol_type": "gadget"},
		{"name": "X", "repo_name": "../etc"},
		{},
	} {
		res, err := deps.handleLookupSymbol(context.Background(), callTool(t, args))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError {
			t.Errorf("invalid arguments %v were accepted", args)
		}
	}
}

func TestGetTypeDefinitionRestrictsToTypeKinds(t *testing.T) {
	cache := &stubCache{records: []model.SymbolRecord{
		sym("nfs-ganesha", "fsal_obj_handle", model.SymbolStruct, model.LanguageC),
		sym("nfs-ganesha", "fsal_obj_handle", model.SymbolFunction, model.LanguageC),
	}}
	deps := newDeps(cache, &stubScanner{}, nil)

	res, err := deps.handleTypeDefinition(context.Background(), callTool(t, map[string]any{
		"type_name": "fsal_obj_handle",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got []model.SymbolRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("payload is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0].SymbolType != model.SymbolStruct {
		t.Fatalf("type lookup returned %+v", got)
	}

	res, err = deps.handleTypeDefinition(context.Background(), callTool(t, map[string]any{"type_name": "missing"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "No indexed type") {
		t.Errorf("a miss must be reported: %s", resultText(t, res))
	}
}

func TestLookupMacroOrConstRestrictsToValueKinds(t *testing.T) {
	cache := &stubCache{records: []model.SymbolRecord{
		sym("auth-service-go", "StatusPending", model.SymbolConstant, model.LanguageGo),
		sym("auth-service-go", "StatusPending", model.SymbolInterface, model.LanguageGo),
	}}
	deps := newDeps(cache, &stubScanner{}, nil)

	res, err := deps.handleMacroOrConst(context.Background(), callTool(t, map[string]any{"name": "StatusPending"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got []model.SymbolRecord
	if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
		t.Fatalf("payload is not a JSON array: %v", err)
	}
	if len(got) != 1 || got[0].SymbolType != model.SymbolConstant {
		t.Fatalf("macro lookup returned %+v", got)
	}
}

func TestReadCodeSnippetValidatesInputAndDelegates(t *testing.T) {
	scanner := &stubScanner{snippet: "     1 | package auth\n"}
	deps := newDeps(&stubCache{}, scanner, nil)

	res, err := deps.handleReadSnippet(context.Background(), callTool(t, map[string]any{
		"repo_name":  "auth-service-go",
		"file_path":  "token.go",
		"start_line": float64(1),
		"end_line":   float64(20),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if got := scanner.gotArgs; len(got) != 4 || got[0] != "auth-service-go" || got[2] != 1 || got[3] != 20 {
		t.Fatalf("arguments were not forwarded verbatim: %v", scanner.gotArgs)
	}
	if !strings.Contains(resultText(t, res), "package auth") {
		t.Errorf("snippet was not returned: %s", resultText(t, res))
	}

	for _, args := range []map[string]any{
		{"file_path": "token.go", "start_line": float64(1), "end_line": float64(2)},
		{"repo_name": "../etc", "file_path": "x", "start_line": float64(1), "end_line": float64(2)},
		{"repo_name": "auth-service-go", "start_line": float64(1), "end_line": float64(2)},
		{"repo_name": "auth-service-go", "file_path": "token.go", "end_line": float64(2)},
	} {
		res, err := deps.handleReadSnippet(context.Background(), callTool(t, args))
		if err != nil {
			t.Fatalf("handler error: %v", err)
		}
		if !res.IsError {
			t.Errorf("invalid arguments %v were accepted", args)
		}
	}

	scanner.snipErr = errors.New("resolves outside repository")
	res, err = deps.handleReadSnippet(context.Background(), callTool(t, map[string]any{
		"repo_name":  "auth-service-go",
		"file_path":  "../../etc/passwd",
		"start_line": float64(1),
		"end_line":   float64(2),
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "resolves outside repository") {
		t.Errorf("traversal error was not surfaced: %s", resultText(t, res))
	}
}

func TestListCapabilitiesMergesCuratedAndObservedFacts(t *testing.T) {
	cache := &stubCache{stats: map[string]store.RepoStat{
		"auth-service-go": {
			RepoName:    "auth-service-go",
			Symbols:     9,
			Functions:   3,
			Languages:   map[string]int{"go": 9},
			SymbolTypes: map[string]int{"struct": 2, "function": 3, "constant": 4},
			Files:       map[string]int{"token.go": 1, "types.go": 1},
			Samples:     []string{"ValidateSessionToken", "Session"},
		},
	}}
	deps := newDeps(cache, &stubScanner{}, []capabilities.Profile{{
		Repo:     "auth-service-go",
		Language: "Go",
		Summary:  "Issues and validates session tokens.",
		Features: []string{"HMAC session tokens"},
	}})

	res, err := deps.handleListCapabilities(context.Background(), callTool(t, map[string]any{
		"repo_name": "auth-service-go",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := resultText(t, res)
	for _, want := range []string{
		"# Repository: auth-service-go",
		"Engineering language stack: Go",
		"Issues and validates session tokens.",
		"HMAC session tokens",
		"Cached symbols: 9 (functions and methods: 3)",
		"Declaration mix:",
		"ValidateSessionToken",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("briefing is missing %q:\n%s", want, text)
		}
	}

	res, err = deps.handleListCapabilities(context.Background(), callTool(t, map[string]any{
		"repo_name": "../../etc",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Error("a traversal repo_name was accepted")
	}

	res, err = deps.handleListCapabilities(context.Background(), callTool(t, map[string]any{
		"repo_name": "ghost-service",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "Known repositories") {
		t.Errorf("an unknown repository must list the known ones: %s", resultText(t, res))
	}
}

func TestLiveScanDisabledSkipsFallback(t *testing.T) {
	scanner := &stubScanner{}
	deps := newDeps(&stubCache{}, scanner, nil)
	deps.LiveScan = false

	res, err := deps.handleLookupSymbol(context.Background(), callTool(t, map[string]any{"name": "anything"}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Error("a miss with live scan disabled must be an error")
	}
	if scanner.calls != 0 {
		t.Errorf("live scan ran %d times while disabled", scanner.calls)
	}
}
