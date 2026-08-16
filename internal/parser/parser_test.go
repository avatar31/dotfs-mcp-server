package parser

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

func parseFixture(t *testing.T, engine Engine, path string) map[string]Symbol {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if !engine.Prefilter(src) {
		t.Fatalf("prefilter rejected %s", path)
	}
	symbols, err := engine.Parse(context.Background(), path, src)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	byName := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		if s.StartLine <= 0 || s.EndLine < s.StartLine {
			t.Errorf("symbol %q has an invalid line range %d-%d", s.Name, s.StartLine, s.EndLine)
		}
		if s.StartByte >= s.EndByte {
			t.Errorf("symbol %q has an invalid byte range %d-%d", s.Name, s.StartByte, s.EndByte)
		}
		if !s.Type.Valid() {
			t.Errorf("symbol %q has an invalid symbol_type %q", s.Name, s.Type)
		}
		byName[s.Name] = s
	}
	return byName
}

func want(t *testing.T, symbols map[string]Symbol, name string) Symbol {
	t.Helper()
	sym, ok := symbols[name]
	if !ok {
		t.Fatalf("symbol %q was not extracted", name)
	}
	return sym
}

func TestGoEngineExtractsFunctionsAndMethods(t *testing.T) {
	symbols := parseFixture(t, NewGoEngine(), "../../testdata/workspace/auth-service-go/token.go")

	fn := want(t, symbols, "ValidateSessionToken")
	if fn.Type != model.SymbolFunction {
		t.Errorf("symbol_type = %q, want %q", fn.Type, model.SymbolFunction)
	}
	if !strings.HasPrefix(fn.SourceCode, "func ValidateSessionToken(") {
		t.Errorf("source does not start at the func keyword: %q", fn.SourceCode)
	}
	if !strings.Contains(fn.Documentation, "verifies the HMAC signature") {
		t.Errorf("documentation was not attached: %q", fn.Documentation)
	}
	if strings.Contains(fn.SourceCode, "//") {
		t.Errorf("doc comment leaked into the isolated source: %q", fn.SourceCode)
	}

	method := want(t, symbols, "Issue")
	if method.Type != model.SymbolMethod {
		t.Errorf("symbol_type = %q, want %q", method.Type, model.SymbolMethod)
	}
	if method.ParentScope != "Issuer" {
		t.Errorf("parent_scope = %q, want %q", method.ParentScope, "Issuer")
	}
	if len(method.Aliases) != 1 || method.Aliases[0] != "Issuer.Issue" {
		t.Errorf("aliases = %v, want [Issuer.Issue]", method.Aliases)
	}
	if method.Signature != "func (i *Issuer) Issue(account string) ([]byte, error)" {
		t.Errorf("signature = %q", method.Signature)
	}
}

func TestGoEngineExtractsTypesInterfacesAndConstants(t *testing.T) {
	symbols := parseFixture(t, NewGoEngine(), "../../testdata/workspace/auth-service-go/types.go")

	st := want(t, symbols, "Session")
	if st.Type != model.SymbolStruct {
		t.Errorf("Session symbol_type = %q, want %q", st.Type, model.SymbolStruct)
	}
	if !strings.HasPrefix(st.SourceCode, "type Session struct {") {
		t.Errorf("struct source is not self-contained: %q", st.SourceCode)
	}
	if !strings.Contains(st.Documentation, "Embedded fields: Metadata") {
		t.Errorf("embedded field was not extracted: %q", st.Documentation)
	}
	if !strings.Contains(st.Documentation, `json:"id" yaml:"id"`) {
		t.Errorf("struct tags were not extracted: %q", st.Documentation)
	}

	iface := want(t, symbols, "SessionStore")
	if iface.Type != model.SymbolInterface {
		t.Errorf("SessionStore symbol_type = %q, want %q", iface.Type, model.SymbolInterface)
	}
	if !strings.Contains(iface.Documentation, "Save(s Session) error") {
		t.Errorf("interface method signatures were not extracted: %q", iface.Documentation)
	}
	if !strings.Contains(iface.Documentation, "Embedded interfaces: Loader") {
		t.Errorf("embedded interface was not extracted: %q", iface.Documentation)
	}

	alias := want(t, symbols, "AccountID")
	if alias.Type != model.SymbolTypeAlias {
		t.Errorf("AccountID symbol_type = %q, want %q", alias.Type, model.SymbolTypeAlias)
	}

	named := want(t, symbols, "SessionState")
	if named.Type != model.SymbolTypedef {
		t.Errorf("SessionState symbol_type = %q, want %q", named.Type, model.SymbolTypedef)
	}

	// Every member of a grouped iota block must carry the whole block so the
	// model can reason about neighbouring values.
	for _, name := range []string{"StatusPending", "StatusActive", "StatusRevoked"} {
		c := want(t, symbols, name)
		if c.Type != model.SymbolConstant {
			t.Errorf("%s symbol_type = %q, want %q", name, c.Type, model.SymbolConstant)
		}
		if !strings.Contains(c.SourceCode, "StatusRevoked") || !strings.Contains(c.SourceCode, "iota") {
			t.Errorf("%s does not carry the const block: %q", name, c.SourceCode)
		}
		if c.ParentScope != "StatusPending block" {
			t.Errorf("%s parent_scope = %q", name, c.ParentScope)
		}
	}
	if doc := want(t, symbols, "StatusActive").Documentation; !strings.Contains(doc, "currently authorising") {
		t.Errorf("per-spec doc comment was not attached: %q", doc)
	}

	// Exported package-level variables are indexed; unexported ones are not.
	tokens := parseFixture(t, NewGoEngine(), "../../testdata/workspace/auth-service-go/token.go")
	if _, ok := tokens["ErrExpired"]; !ok {
		t.Error("exported package variable ErrExpired was not indexed")
	}
}

func TestCEngineExtractsMacrosRecordsEnumsAndTypedefs(t *testing.T) {
	symbols := parseFixture(t, NewCEngine(), "../../testdata/workspace/packet-router-c/router.h")

	macro := want(t, symbols, "ROUTER_QUEUE_DEPTH")
	if macro.Type != model.SymbolMacro {
		t.Errorf("symbol_type = %q, want %q", macro.Type, model.SymbolMacro)
	}
	if macro.Signature != "#define ROUTER_QUEUE_DEPTH 1024" {
		t.Errorf("macro signature = %q", macro.Signature)
	}
	if macro.StartLine != macro.EndLine {
		t.Errorf("single-line macro spans %d-%d", macro.StartLine, macro.EndLine)
	}

	fnMacro := want(t, symbols, "ROUTER_MIN")
	if fnMacro.Type != model.SymbolMacroFunction {
		t.Errorf("symbol_type = %q, want %q", fnMacro.Type, model.SymbolMacroFunction)
	}
	if fnMacro.Signature != "#define ROUTER_MIN(a, b)" {
		t.Errorf("macro function signature = %q", fnMacro.Signature)
	}

	vtable := want(t, symbols, "router_ops")
	if vtable.Type != model.SymbolStruct {
		t.Errorf("symbol_type = %q, want %q", vtable.Type, model.SymbolStruct)
	}
	if !strings.Contains(vtable.Documentation, "Fields: open, write, close") {
		t.Errorf("function table members were not extracted: %q", vtable.Documentation)
	}

	un := want(t, symbols, "router_addr")
	if un.Signature != "union router_addr" {
		t.Errorf("union signature = %q", un.Signature)
	}

	enum := want(t, symbols, "router_priority")
	if enum.Type != model.SymbolEnum {
		t.Errorf("symbol_type = %q, want %q", enum.Type, model.SymbolEnum)
	}
	low := want(t, symbols, "ROUTER_PRIORITY_LOW")
	if low.Type != model.SymbolConstant || low.ParentScope != "router_priority" {
		t.Errorf("enumerator = %q/%q", low.Type, low.ParentScope)
	}

	td := want(t, symbols, "router_status_t")
	if td.Type != model.SymbolTypedef {
		t.Errorf("symbol_type = %q, want %q", td.Type, model.SymbolTypedef)
	}
	// Enumerators of an anonymous enum inherit the typedef name as their scope.
	quota := want(t, symbols, "ROUTER_ERR_NO_QUOTA")
	if quota.ParentScope != "router_status_t" {
		t.Errorf("anonymous enumerator scope = %q", quota.ParentScope)
	}
	if !strings.Contains(quota.SourceCode, "ROUTER_ERR_TRUNCATED") {
		t.Errorf("enumerator does not carry the enum block: %q", quota.SourceCode)
	}

	proto := want(t, symbols, "route_packet")
	if proto.Type != model.SymbolFunction {
		t.Errorf("prototype symbol_type = %q", proto.Type)
	}
	if strings.HasSuffix(proto.Signature, ";") {
		t.Errorf("prototype signature keeps the semicolon: %q", proto.Signature)
	}
}

func TestCEngineIgnoresNonDefinitions(t *testing.T) {
	symbols := parseFixture(t, NewCEngine(), "../../testdata/workspace/packet-router-c/router.c")

	fn := want(t, symbols, "read_session_header")
	if !strings.HasPrefix(fn.SourceCode, "int read_session_header(") {
		t.Errorf("source does not start at the definition: %q", fn.SourceCode)
	}
	if !strings.Contains(fn.Documentation, "copies the fixed 32-byte session header") {
		t.Errorf("block comment was not attached: %q", fn.Documentation)
	}
	if strings.Contains(fn.Documentation, "*") || strings.Contains(fn.Documentation, "/*") {
		t.Errorf("comment markers leaked into the documentation: %q", fn.Documentation)
	}

	// route_packet prints the string "read_session_header"; only one genuine
	// definition of that identifier may be extracted.
	router := want(t, symbols, "route_packet")
	if !strings.Contains(router.SourceCode, "printf") {
		t.Errorf("route_packet body was truncated: %q", router.SourceCode)
	}
	if fn.StartByte == router.StartByte {
		t.Error("two distinct definitions share a start offset")
	}
}

func TestRegistryRoutesByExtension(t *testing.T) {
	r := NewDefaultRegistry()

	for path, lang := range map[string]model.Language{
		"/w/repo/main.go":     model.LanguageGo,
		"/w/repo/router.c":    model.LanguageC,
		"/w/repo/router.h":    model.LanguageC,
		"/w/repo/Router.H":    model.LanguageC,
		"/w/repo/nested/x.go": model.LanguageGo,
	} {
		engine, ok := r.For(path)
		if !ok {
			t.Fatalf("no engine registered for %s", path)
		}
		if engine.Language() != lang {
			t.Errorf("%s routed to %q, want %q", path, engine.Language(), lang)
		}
	}

	for _, path := range []string{"/w/repo/README.md", "/w/repo/Makefile", "/w/repo/build.rs"} {
		if r.Supports(path) {
			t.Errorf("%s must not be parseable", path)
		}
	}
}

func TestParseRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, engine := range []Engine{NewGoEngine(), NewCEngine()} {
		if _, err := engine.Parse(ctx, "x", []byte("package x\n")); err == nil {
			t.Errorf("%T.Parse ignored a cancelled context", engine)
		}
	}
}

func TestCleanCommentStripsMarkers(t *testing.T) {
	cases := map[string]string{
		"// one\n// two":     "one\ntwo",
		"/// doc":            "doc",
		"/*\n * body\n */":   "body",
		"/* single line */":  "single line",
		"\n\n// padded\n\n":  "padded",
		"":                   "",
		"// keep\n\n// gaps": "keep\n\ngaps",
	}
	for in, expect := range cases {
		if got := cleanComment(in); got != expect {
			t.Errorf("cleanComment(%q) = %q, want %q", in, got, expect)
		}
	}
}
