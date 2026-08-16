package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

const goSource = `package auth

// ValidateSessionToken verifies the HMAC signature.
//
// It mirrors read_session_header() on the C side.
func ValidateSessionToken(raw []byte) (string, error) {
	return "", nil
}

// Issue signs a new session token.
func (i *Issuer) Issue(account string) ([]byte, error) {
	return nil, nil
}
`

const cSource = `#include <stdio.h>

/*
 * read_session_header copies the session header.
 * Returns 0 on success.
 */
int read_session_header(const unsigned char *frame)
{
    return 0;
}

// route_packet dispatches a frame.
int route_packet(const unsigned char *frame)
{
    printf("read_session_header failed\n");
    return read_session_header(frame);
}
`

func TestGoEngineExtractsFunctionsAndDocumentation(t *testing.T) {
	fns, err := NewGoEngine().Parse(context.Background(), "token.go", []byte(goSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("want 2 functions, got %d", len(fns))
	}

	validate := fns[0]
	if validate.Name != "ValidateSessionToken" {
		t.Fatalf("want ValidateSessionToken, got %q", validate.Name)
	}
	if validate.Language != model.LanguageGo {
		t.Fatalf("want language go, got %q", validate.Language)
	}
	if strings.Contains(validate.Documentation, "//") {
		t.Fatalf("documentation is not boilerplate-free: %q", validate.Documentation)
	}
	if !strings.HasPrefix(validate.Documentation, "ValidateSessionToken verifies") {
		t.Fatalf("unexpected documentation: %q", validate.Documentation)
	}
	if !strings.HasPrefix(validate.SourceCode, "func ValidateSessionToken(") {
		t.Fatalf("source must start at the func keyword, got %q", validate.SourceCode)
	}
	if strings.Contains(validate.SourceCode, "// ValidateSessionToken verifies") {
		t.Fatalf("doc comment must not be part of the isolated source body")
	}
}

func TestGoEngineExposesReceiverQualifiedAlias(t *testing.T) {
	fns, err := NewGoEngine().Parse(context.Background(), "token.go", []byte(goSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	method := fns[1]
	if method.Name != "Issue" {
		t.Fatalf("want Issue, got %q", method.Name)
	}
	if len(method.Aliases) != 1 || method.Aliases[0] != "Issuer.Issue" {
		t.Fatalf("want alias Issuer.Issue, got %v", method.Aliases)
	}
}

func TestGoEngineRejectsBrokenSource(t *testing.T) {
	if _, err := NewGoEngine().Parse(context.Background(), "broken.go", []byte("package ;;")); err == nil {
		t.Fatal("want a parse error for malformed Go source")
	}
}

func TestCEngineExtractsDefinitionsWithLeadingComments(t *testing.T) {
	fns, err := NewCEngine().Parse(context.Background(), "router.c", []byte(cSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(fns) != 2 {
		t.Fatalf("want 2 function definitions, got %d", len(fns))
	}

	header := fns[0]
	if header.Name != "read_session_header" {
		t.Fatalf("want read_session_header, got %q", header.Name)
	}
	if header.Language != model.LanguageC {
		t.Fatalf("want language c, got %q", header.Language)
	}
	if strings.ContainsAny(header.Documentation, "*/") {
		t.Fatalf("documentation still carries comment markers: %q", header.Documentation)
	}
	if !strings.HasPrefix(header.Documentation, "read_session_header copies") {
		t.Fatalf("unexpected documentation: %q", header.Documentation)
	}
	if !strings.HasPrefix(header.SourceCode, "int read_session_header(") {
		t.Fatalf("unexpected source isolation: %q", header.SourceCode)
	}
}

// The literal "read_session_header" also appears inside a printf call and a
// call expression. Only the genuine function_definition node may be indexed.
func TestCEngineIgnoresNonDefinitionOccurrences(t *testing.T) {
	fns, err := NewCEngine().Parse(context.Background(), "router.c", []byte(cSource))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	seen := map[string]int{}
	for _, fn := range fns {
		seen[fn.Name]++
	}
	if seen["read_session_header"] != 1 {
		t.Fatalf("want exactly one read_session_header definition, got %d", seen["read_session_header"])
	}
	if seen["route_packet"] != 1 {
		t.Fatalf("want exactly one route_packet definition, got %d", seen["route_packet"])
	}
}

func TestRegistryRoutesByExtension(t *testing.T) {
	reg := NewDefaultRegistry()

	cases := map[string]model.Language{
		"a/b/main.go":  model.LanguageGo,
		"a/b/router.c": model.LanguageC,
		"a/b/router.h": model.LanguageC,
	}
	for path, want := range cases {
		engine, ok := reg.For(path)
		if !ok {
			t.Fatalf("no engine registered for %q", path)
		}
		if engine.Language() != want {
			t.Fatalf("%q routed to %q, want %q", path, engine.Language(), want)
		}
	}

	if reg.Supports("a/b/readme.md") {
		t.Fatal("markdown must not be routed to an AST engine")
	}
}

func TestPrefilterRejectsIrrelevantFiles(t *testing.T) {
	if NewGoEngine().Prefilter([]byte("package main\n\nvar x = 1\n")) {
		t.Fatal("a Go file without the func keyword must be filtered out")
	}
	if NewCEngine().Prefilter([]byte("#define A 1\n")) {
		t.Fatal("a C file without parentheses must be filtered out")
	}
}
