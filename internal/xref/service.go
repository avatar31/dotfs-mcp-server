package xref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
	"github.com/avatar31/dotfs-mcp-server/internal/model"
	"github.com/avatar31/dotfs-mcp-server/internal/utils"
)

// ErrTimeout signals that a language server blew the per-call time budget. The
// tool layer surfaces it verbatim so the agent can retry or degrade to Phase 2.
var ErrTimeout = errors.New("xref: the language server exceeded its time budget")

// Session is the slice of an LSP client the service depends on. Keeping it an
// interface makes the whole compaction pipeline testable without a daemon.
type Session interface {
	EnsureOpen(ctx context.Context, path, languageID string) error
	Call(ctx context.Context, method string, params, out any) error
}

// Provider hands out initialised sessions, one per repository and language.
type Provider interface {
	ClientSession(ctx context.Context, repo, repoDir string, lang model.Language) (Session, error)
	RequestTimeout() time.Duration
}

// target is a fully validated query anchor.
type target struct {
	repo     string
	relPath  string
	absPath  string
	session  Session
	position lsp.Position
	doc      lsp.TextDocumentIdentifier
	name     string
	src      *sourceCache
}

// params builds the positional request body shared by every LSP method.
func (t target) params() lsp.TextDocumentPositionParams {
	return lsp.TextDocumentPositionParams{TextDocument: t.doc, Position: t.position}
}

// Service answers relational queries by driving a language server and then
// compacting whatever it says.
type Service struct {
	provider Provider
	root     string
	log      *slog.Logger
	limit    int
	timeout  time.Duration
}

// New builds the service. workspaceRoot must be the same directory the indexer
// walks, because every result is reported relative to it.
func New(provider Provider, workspaceRoot string, log *slog.Logger) (*Service, error) {
	if provider == nil {
		return nil, errors.New("xref: a session provider is required")
	}
	if log == nil {
		return nil, errors.New("xref: a logger is required")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("xref: resolve workspace root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	timeout := provider.RequestTimeout()
	if timeout <= 0 {
		timeout = lsp.DefaultRequestTimeout
	}
	return &Service{provider: provider, root: root, log: log, limit: MaxResults, timeout: timeout}, nil
}

// resolve validates the request, proves the file lives inside the repository,
// starts or reuses the right daemon and publishes the document to it.
func (s *Service) resolve(ctx context.Context, p Position) (*target, error) {
	if err := utils.ValidateRepoName(p.Repo); err != nil {
		return nil, fmt.Errorf("xref: %w", err)
	}
	if p.Line < 1 {
		return nil, fmt.Errorf("xref: line must be a positive 1-based number, got %d", p.Line)
	}
	if p.Character < 1 {
		return nil, fmt.Errorf("xref: character must be a positive 1-based number, got %d", p.Character)
	}

	repoDir, err := utils.SafeRepoPath(s.root, p.Repo)
	if err != nil {
		return nil, fmt.Errorf("xref: %w", err)
	}
	abs, rel, err := resolveInRepo(repoDir, p.FilePath)
	if err != nil {
		return nil, err
	}

	lang, err := utils.LanguageFor(abs)
	if err != nil {
		return nil, fmt.Errorf("xref: %w", err)
	}

	session, err := s.provider.ClientSession(ctx, p.Repo, repoDir, lang)
	if err != nil {
		return nil, err
	}
	if err := session.EnsureOpen(ctx, abs, utils.LanguageID(lang, abs)); err != nil {
		return nil, err
	}

	src := newSourceCache()
	return &target{
		repo:     p.Repo,
		relPath:  rel,
		absPath:  abs,
		session:  session,
		position: lsp.Position{Line: p.Line - 1, Character: p.Character - 1},
		doc:      lsp.TextDocumentIdentifier{URI: lsp.PathToURI(abs)},
		name:     src.identifierAt(abs, p.Line, p.Character),
		src:      src,
	}, nil
}

// call bounds one round trip and normalises a deadline into ErrTimeout.
func (s *Service) call(ctx context.Context, sess Session, method string, params, out any) error {
	bounded, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	err := sess.Call(bounded, method, params, out)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return fmt.Errorf("%w: %s took longer than %s", ErrTimeout, method, s.timeout)
	default:
		return err
	}
}

// FindReferences resolves every usage of the symbol under the given position.
func (s *Service) FindReferences(ctx context.Context, req ReferenceRequest) (ReferenceResult, error) {
	t, err := s.resolve(ctx, req.Position)
	if err != nil {
		return ReferenceResult{}, err
	}

	var locations []lsp.Location
	params := lsp.ReferenceParams{
		TextDocumentPositionParams: t.params(),
		Context:                    lsp.ReferenceContext{IncludeDeclaration: req.IncludeDeclaration},
	}
	if err := s.call(ctx, t.session, lsp.MethodReferences, params, &locations); err != nil {
		return ReferenceResult{}, err
	}

	kept, total := s.compactLocations(locations, t.src, false)
	result := ReferenceResult{
		Symbol:          t.name,
		Repo:            t.repo,
		FilePath:        t.relPath,
		TotalReferences: total,
		Truncated:       total > len(kept),
		References:      kept,
	}
	if total == 0 {
		result.References = []Reference{}
		result.Hint = fallbackHint
	}
	return result, nil
}

// CallHierarchy computes the incoming or outgoing call edges of the function
// enclosing the given position.
func (s *Service) CallHierarchy(ctx context.Context, req CallHierarchyRequest) (CallHierarchyResult, error) {
	direction := strings.ToLower(strings.TrimSpace(req.Direction))
	if direction != DirectionIncoming && direction != DirectionOutgoing {
		return CallHierarchyResult{}, fmt.Errorf("xref: direction must be %q or %q, got %q",
			DirectionIncoming, DirectionOutgoing, req.Direction)
	}

	t, err := s.resolve(ctx, req.Position)
	if err != nil {
		return CallHierarchyResult{}, err
	}

	var items []lsp.CallHierarchyItem
	if err := s.call(ctx, t.session, lsp.MethodPrepareCallHierarchy, t.params(), &items); err != nil {
		return CallHierarchyResult{}, err
	}
	if len(items) == 0 {
		return CallHierarchyResult{
			Symbol:    t.name,
			Direction: direction,
			Calls:     []Call{},
			Hint:      fallbackHint,
		}, nil
	}

	root := items[0]
	result := CallHierarchyResult{Symbol: root.Name, Direction: direction, Calls: []Call{}}
	if result.Symbol == "" {
		result.Symbol = t.name
	}

	var edges []Call
	var total int
	if direction == DirectionIncoming {
		edges, total, err = s.incoming(ctx, t, root)
		result.TotalCallers = total
	} else {
		edges, total, err = s.outgoing(ctx, t, root)
		result.TotalCallees = total
	}
	if err != nil {
		return CallHierarchyResult{}, err
	}

	result.Calls = edges
	result.Truncated = total > len(edges)
	if total == 0 {
		result.Hint = fallbackHint
	}
	return result, nil
}

// incoming reports the call sites that reach the root item. The reported line
// is the call site itself, not the caller's declaration, which is what an
// engineer tracing a bug actually needs.
func (s *Service) incoming(ctx context.Context, t *target, root lsp.CallHierarchyItem) ([]Call, int, error) {
	var calls []lsp.CallHierarchyIncomingCall
	// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchy_incomingCalls
	if err := s.call(ctx, t.session, lsp.MethodIncomingCalls, lsp.CallHierarchyItemParams{Item: root}, &calls); err != nil {
		return nil, 0, err
	}

	seen := make(map[string]struct{})
	out := make([]Call, 0, min(len(calls), s.limit))
	total := 0

	for _, call := range calls {
		sites := call.FromRanges
		if len(sites) == 0 {
			sites = []lsp.Range{call.From.SelectionRange}
		}
		for _, site := range sites {
			ref, ok := s.reference(call.From.URI, site.Start, t.src)
			if !ok {
				continue
			}
			key := fmt.Sprintf("%s:%d", ref.FilePath, ref.Line)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			total++
			if len(out) >= s.limit {
				continue
			}
			out = append(out, Call{
				CallerName: call.From.Name,
				Repo:       ref.Repo,
				FilePath:   ref.FilePath,
				Line:       ref.Line,
				Snippet:    ref.Snippet,
			})
		}
	}
	return out, total, nil
}

// outgoing reports the functions the root item calls, located at their own
// definition so the agent can navigate straight to the implementation.
func (s *Service) outgoing(ctx context.Context, t *target, root lsp.CallHierarchyItem) ([]Call, int, error) {
	var calls []lsp.CallHierarchyOutgoingCall
	// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#callHierarchy_outgoingCalls
	if err := s.call(ctx, t.session, lsp.MethodOutgoingCalls, lsp.CallHierarchyItemParams{Item: root}, &calls); err != nil {
		return nil, 0, err
	}

	seen := make(map[string]struct{})
	out := make([]Call, 0, min(len(calls), s.limit))
	total := 0

	for _, call := range calls {
		ref, ok := s.reference(call.To.URI, call.To.SelectionRange.Start, t.src)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s:%d", ref.FilePath, ref.Line)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		total++
		if len(out) >= s.limit {
			continue
		}
		out = append(out, Call{
			CalleeName: call.To.Name,
			Repo:       ref.Repo,
			FilePath:   ref.FilePath,
			Line:       ref.Line,
			Snippet:    ref.Snippet,
		})
	}
	return out, total, nil
}

// Implementations maps an interface (Go) or a v-table style declaration (C)
// onto the concrete types that satisfy it.
func (s *Service) Implementations(ctx context.Context, p Position) (ImplementationResult, error) {
	t, err := s.resolve(ctx, p)
	if err != nil {
		return ImplementationResult{}, err
	}

	// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#textDocument_implementation
	var raw json.RawMessage
	if err := s.call(ctx, t.session, lsp.MethodImplementation, t.params(), &raw); err != nil {
		if lsp.IsMethodNotFound(err) {
			return ImplementationResult{
				Symbol:          t.name,
				Implementations: []Reference{},
				Hint: "This language server does not implement textDocument/implementation. " +
					"For C function-pointer tables, use find_references on the struct field instead.",
			}, nil
		}
		return ImplementationResult{}, err
	}

	locations, err := decodeLocations(raw)
	if err != nil {
		return ImplementationResult{}, err
	}

	kept, total := s.compactLocations(locations, t.src, true)
	result := ImplementationResult{
		Symbol:               t.name,
		TotalImplementations: total,
		Truncated:            total > len(kept),
		Implementations:      kept,
	}
	if total == 0 {
		result.Implementations = []Reference{}
		result.Hint = fallbackHint
	}
	return result, nil
}

// TypeHierarchy resolves the declaration of a type plus its supertypes and
// subtypes. Plain C has no inheritance, so clangd answers the hierarchy half
// only for C++ translation units; the definition lookup always works and is
// what makes the tool useful on a C codebase.
func (s *Service) TypeHierarchy(ctx context.Context, req TypeHierarchyRequest) (TypeHierarchyResult, error) {
	direction := strings.ToLower(strings.TrimSpace(req.Direction))
	if direction == "" {
		direction = DirectionBoth
	}
	if direction != DirectionSupertypes && direction != DirectionSubtypes && direction != DirectionBoth {
		return TypeHierarchyResult{}, fmt.Errorf("xref: direction must be %q, %q or %q, got %q",
			DirectionSupertypes, DirectionSubtypes, DirectionBoth, req.Direction)
	}

	t, err := s.resolve(ctx, req.Position)
	if err != nil {
		return TypeHierarchyResult{}, err
	}

	result := TypeHierarchyResult{Symbol: t.name}
	if def := s.typeDefinition(ctx, t); def != nil {
		result.Definition = def
	}

	// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#textDocument_prepareTypeHierarchy
	var items []lsp.TypeHierarchyItem
	if err := s.call(ctx, t.session, lsp.MethodPrepareTypeHierarchy, t.params(), &items); err != nil {
		if !lsp.IsMethodNotFound(err) {
			return TypeHierarchyResult{}, err
		}
		s.log.Debug("type hierarchy unsupported by this server", "repo", t.repo)
	}

	if len(items) == 0 {
		if result.Definition == nil {
			result.Hint = fallbackHint
		} else {
			result.Hint = "The language server exposes no type hierarchy at this position " +
				"(expected for plain C structs). The declaration site is reported above."
		}
		return result, nil
	}

	if items[0].Name != "" {
		result.Symbol = items[0].Name
	}
	if direction == DirectionSupertypes || direction == DirectionBoth {
		// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#typeHierarchy_supertypes
		relatives, total, err := s.relatives(ctx, t, lsp.MethodSupertypes, items[0])
		if err != nil {
			return TypeHierarchyResult{}, err
		}
		result.Supertypes, result.TotalSupertypes = relatives, total
	}
	if direction == DirectionSubtypes || direction == DirectionBoth {
		// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#typeHierarchy_subtypes
		relatives, total, err := s.relatives(ctx, t, lsp.MethodSubtypes, items[0])
		if err != nil {
			return TypeHierarchyResult{}, err
		}
		result.Subtypes, result.TotalSubtypes = relatives, total
	}
	return result, nil
}

// typeDefinition resolves the declaration site, falling back from
// textDocument/typeDefinition to textDocument/definition.
func (s *Service) typeDefinition(ctx context.Context, t *target) *Reference {
	for _, method := range []string{lsp.MethodTypeDefinition, lsp.MethodDefinition} {
		// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#textDocument_typeDefinition
		var raw json.RawMessage
		if err := s.call(ctx, t.session, method, t.params(), &raw); err != nil {
			s.log.Debug("definition lookup failed", "method", method, "error", err)
			continue
		}
		locations, err := decodeLocations(raw)
		if err != nil || len(locations) == 0 {
			continue
		}
		ref, ok := s.reference(locations[0].URI, locations[0].Range.Start, t.src)
		if !ok {
			continue
		}
		if abs, err := locations[0].URI.Path(); err == nil {
			ref.Name = t.src.identifierAt(abs, ref.Line, ref.Character)
		}
		return &ref
	}
	return nil
}

// relatives fetches one side of the type hierarchy.
func (s *Service) relatives(ctx context.Context, t *target, method string, item lsp.TypeHierarchyItem) ([]Reference, int, error) {
	var items []lsp.TypeHierarchyItem
	if err := s.call(ctx, t.session, method, lsp.TypeHierarchyItemParams{Item: item}, &items); err != nil {
		if lsp.IsMethodNotFound(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}

	out := make([]Reference, 0, min(len(items), s.limit))
	for _, related := range items {
		if len(out) >= s.limit {
			break
		}
		ref, ok := s.reference(related.URI, related.SelectionRange.Start, t.src)
		if !ok {
			continue
		}
		ref.Name = related.Name
		ref.Kind = related.Kind.String()
		out = append(out, ref)
	}
	return out, len(items), nil
}

// resolveInRepo proves a caller-supplied relative path stays inside the
// repository, mirroring the guarantees of the Phase 2 snippet reader.
func resolveInRepo(repoDir, relPath string) (abs string, clean string, err error) {
	trimmed := strings.TrimSpace(relPath)
	if trimmed == "" {
		return "", "", errors.New("xref: file_path must not be empty")
	}
	native := filepath.FromSlash(trimmed)
	if filepath.IsAbs(native) {
		return "", "", fmt.Errorf("xref: file_path %q must be relative to the repository", relPath)
	}
	cleaned := filepath.Clean(native)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("xref: file_path %q must not escape the repository", relPath)
	}

	candidate := filepath.Join(repoDir, cleaned)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", fmt.Errorf("xref: resolve %q: %w", relPath, err)
	}
	realRepo, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		return "", "", fmt.Errorf("xref: resolve repository directory: %w", err)
	}
	rel, err := filepath.Rel(realRepo, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("xref: file_path %q resolves outside the repository", relPath)
	}
	return resolved, filepath.ToSlash(rel), nil
}
