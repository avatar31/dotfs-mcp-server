package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
	"github.com/avatar31/dotfs-mcp-server/internal/utils"
	"github.com/avatar31/dotfs-mcp-server/internal/xref"
)

type stubXRef struct {
	refResult   xref.ReferenceResult
	callResult  xref.CallHierarchyResult
	implResult  xref.ImplementationResult
	typeResult  xref.TypeHierarchyResult
	err         error
	lastRefReq  xref.ReferenceRequest
	lastCallReq xref.CallHierarchyRequest
	lastPos     xref.Position
	lastTypeReq xref.TypeHierarchyRequest
	calls       int
}

func (s *stubXRef) FindReferences(_ context.Context, req xref.ReferenceRequest) (xref.ReferenceResult, error) {
	s.calls++
	s.lastRefReq = req
	return s.refResult, s.err
}

func (s *stubXRef) CallHierarchy(_ context.Context, req xref.CallHierarchyRequest) (xref.CallHierarchyResult, error) {
	s.calls++
	s.lastCallReq = req
	return s.callResult, s.err
}

func (s *stubXRef) Implementations(_ context.Context, pos xref.Position) (xref.ImplementationResult, error) {
	s.calls++
	s.lastPos = pos
	return s.implResult, s.err
}

func (s *stubXRef) TypeHierarchy(_ context.Context, req xref.TypeHierarchyRequest) (xref.TypeHierarchyResult, error) {
	s.calls++
	s.lastTypeReq = req
	return s.typeResult, s.err
}

func positionArguments() map[string]any {
	return map[string]any{
		"repo_name": "nfs-ganesha",
		"file_path": "src/FSAL/fsal_open.c",
		"line":      float64(142),
		"character": float64(11),
	}
}

func withXRef(t *testing.T, x CrossReference) Deps {
	deps := newDeps(t, &stubCache{}, &stubScanner{}, nil)
	deps.XRef = x
	return deps
}

func TestRelationalToolsAreOnlyRegisteredWithAnEngine(t *testing.T) {
	if _, err := New(withXRef(t, &stubXRef{})); err != nil {
		t.Fatalf("new server with the lsp engine: %v", err)
	}
	if _, err := New(newDeps(t, &stubCache{}, &stubScanner{}, nil)); err != nil {
		t.Fatalf("new server without the lsp engine: %v", err)
	}
}

func TestFindReferencesForwardsArgumentsAndMinifies(t *testing.T) {
	engine := &stubXRef{refResult: xref.ReferenceResult{
		Symbol:          "fsal_open",
		Repo:            "nfs-ganesha",
		FilePath:        "src/FSAL/fsal_open.c",
		TotalReferences: 2,
		References: []xref.Reference{
			{Repo: "nfs-ganesha", FilePath: "src/Protocols/NFS/nfs4_op_open.c", Line: 142, Snippet: "status = fsal_open(obj);"},
			{Repo: "nfs-ganesha", FilePath: "src/FSAL/FSAL_VFS/file.c", Line: 88, Snippet: "return sub_fsal->fsal_open(sub_obj);"},
		},
	}}
	deps := withXRef(t, engine)

	args := positionArguments()
	args["include_declaration"] = true
	res, err := deps.handleFindReferences(context.Background(), callTool(t, args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}

	got := engine.lastRefReq
	if got.Repo != "nfs-ganesha" || got.FilePath != "src/FSAL/fsal_open.c" || got.Line != 142 || got.Character != 11 {
		t.Errorf("coordinates were not forwarded verbatim: %+v", got.Position)
	}
	if !got.IncludeDeclaration {
		t.Error("include_declaration was dropped")
	}

	text := resultText(t, res)
	if strings.Contains(text, "\n") || strings.Contains(text, "  ") {
		t.Errorf("relational payloads must be minified: %q", text)
	}
	var decoded xref.ReferenceResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded.TotalReferences != 2 || len(decoded.References) != 2 {
		t.Errorf("payload lost data: %+v", decoded)
	}
	if !strings.Contains(text, `"total_references":2`) {
		t.Errorf("snake_case contract broken: %s", text)
	}
}

func TestFindReferencesDefaultsIncludeDeclarationToFalse(t *testing.T) {
	engine := &stubXRef{}
	deps := withXRef(t, engine)

	if _, err := deps.handleFindReferences(context.Background(), callTool(t, positionArguments())); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if engine.lastRefReq.IncludeDeclaration {
		t.Error("include_declaration must default to false")
	}
}

func TestRelationalToolsValidateCoordinates(t *testing.T) {
	deps := withXRef(t, &stubXRef{})

	missing := []map[string]any{
		{},
		{"repo_name": "nfs-ganesha"},
		{"repo_name": "nfs-ganesha", "file_path": "a.c"},
		{"repo_name": "nfs-ganesha", "file_path": "a.c", "line": float64(3)},
		{"repo_name": "../etc", "file_path": "a.c", "line": float64(3), "character": float64(1)},
		{"repo_name": "nfs-ganesha", "file_path": "   ", "line": float64(3), "character": float64(1)},
		{"repo_name": "nfs-ganesha", "file_path": "a.c", "line": "twelve", "character": float64(1)},
	}

	handlers := map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"find_references":                deps.handleFindReferences,
		"find_interface_implementations": deps.handleImplementations,
		"get_type_hierarchy":             deps.handleTypeHierarchy,
	}
	for name, handler := range handlers {
		for i, args := range missing {
			res, err := handler(context.Background(), callTool(t, args))
			if err != nil {
				t.Fatalf("%s handler error: %v", name, err)
			}
			if !res.IsError {
				t.Errorf("%s accepted invalid arguments #%d: %v", name, i, args)
			}
		}
	}
}

func TestCallHierarchyRequiresADirection(t *testing.T) {
	engine := &stubXRef{callResult: xref.CallHierarchyResult{Symbol: "fsal_open", Direction: "incoming"}}
	deps := withXRef(t, engine)

	res, err := deps.handleCallHierarchy(context.Background(), callTool(t, positionArguments()))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Error("a missing direction was accepted")
	}
	if engine.calls != 0 {
		t.Error("the engine was invoked despite invalid arguments")
	}

	args := positionArguments()
	args["direction"] = "incoming"
	res, err = deps.handleCallHierarchy(context.Background(), callTool(t, args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if engine.lastCallReq.Direction != "incoming" {
		t.Errorf("direction = %q", engine.lastCallReq.Direction)
	}
}

func TestTypeHierarchyDefaultsToBothDirections(t *testing.T) {
	engine := &stubXRef{}
	deps := withXRef(t, engine)

	if _, err := deps.handleTypeHierarchy(context.Background(), callTool(t, positionArguments())); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if engine.lastTypeReq.Direction != xref.DirectionBoth {
		t.Errorf("direction default = %q", engine.lastTypeReq.Direction)
	}
}

func TestImplementationsForwardsThePosition(t *testing.T) {
	engine := &stubXRef{implResult: xref.ImplementationResult{Symbol: "SessionStore", TotalImplementations: 1}}
	deps := withXRef(t, engine)

	res, err := deps.handleImplementations(context.Background(), callTool(t, positionArguments()))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, res))
	}
	if engine.lastPos.Line != 142 || engine.lastPos.Character != 11 {
		t.Errorf("position = %+v", engine.lastPos)
	}
	if !strings.Contains(resultText(t, res), `"total_implementations":1`) {
		t.Errorf("payload = %s", resultText(t, res))
	}
}

func TestEngineFailuresBecomeActionableGuidance(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"timeout":        {xref.ErrTimeout, "Tool execution timeout"},
		"deadline":       {context.DeadlineExceeded, "Tool execution timeout"},
		"no compile db":  {fmt.Errorf("wrapped: %w", lsp.ErrNoCompileCommands), "CMAKE_EXPORT_COMPILE_COMMANDS"},
		"no go module":   {lsp.ErrNoGoModule, "no go.mod"},
		"bad language":   {utils.ErrUnsupportedLanguage, "No language server handles"},
		"daemon crash":   {lsp.ErrDaemonExited, "crashed while answering"},
		"unclassified":   {errors.New("boom"), "boom"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deps := withXRef(t, &stubXRef{err: tc.err})
			res, err := deps.handleFindReferences(context.Background(), callTool(t, positionArguments()))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if !res.IsError {
				t.Fatal("an engine failure must be reported as a tool error")
			}
			text := resultText(t, res)
			if !strings.Contains(text, tc.want) {
				t.Errorf("guidance %q does not mention %q", text, tc.want)
			}
			// Every failure has to leave the agent with a way forward.
			if !strings.Contains(text, "lookup_symbol") && !strings.Contains(text, "global_codebase_search") &&
				!strings.Contains(text, "retry") && !strings.Contains(text, "boom") {
				t.Errorf("no fallback advice in %q", text)
			}
		})
	}
}
