// Package mcpserver exposes the codebase intelligence tools to the LLM client
// over the Model Context Protocol stdio transport.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/avatar31/dotfs-mcp-server/internal/capabilities"
	"github.com/avatar31/dotfs-mcp-server/internal/indexer"
	"github.com/avatar31/dotfs-mcp-server/internal/model"
	"github.com/avatar31/dotfs-mcp-server/internal/store"
)

// liveScanTimeout bounds the real-time fallback so a cache miss can never hang
// the LLM client on a huge workspace.
const liveScanTimeout = 30 * time.Second

// maxResults caps every array-returning tool.
const maxResults = 25

// Cache is the read/lookup surface required from the storage layer.
type Cache interface {
	Lookup(name string, filter store.Filter) ([]model.SymbolRecord, error)
	Stats() (map[string]store.RepoStat, error)
}

// LiveScanner is the workspace-backed fallback and file reader.
type LiveScanner interface {
	SearchLive(ctx context.Context, target string, types []model.SymbolType) ([]model.SymbolRecord, error)
	ListRepos() ([]string, error)
	ReadSnippet(repo, relPath string, startLine, endLine int) (string, error)
}

// Deps are the collaborators injected into the tool handlers.
type Deps struct {
	Cache    Cache
	Scanner  LiveScanner
	Matrix   *capabilities.Matrix
	Log      *slog.Logger
	Name     string
	Version  string
	LiveScan bool
}

// New builds the MCP server and registers the mandatory toolkit.
func New(deps Deps) (*mcpsrv.MCPServer, error) {
	if deps.Cache == nil || deps.Scanner == nil || deps.Matrix == nil || deps.Log == nil {
		return nil, errors.New("mcpserver: cache, scanner, matrix and logger are required")
	}

	srv := mcpsrv.NewMCPServer(
		deps.Name,
		deps.Version,
		mcpsrv.WithToolCapabilities(false),
		mcpsrv.WithRecovery(),
	)

	srv.AddTool(searchTool(), deps.handleGlobalSearch)
	srv.AddTool(lookupSymbolTool(), deps.handleLookupSymbol)
	srv.AddTool(typeDefinitionTool(), deps.handleTypeDefinition)
	srv.AddTool(macroOrConstTool(), deps.handleMacroOrConst)
	srv.AddTool(readSnippetTool(), deps.handleReadSnippet)
	srv.AddTool(capabilitiesTool(), deps.handleListCapabilities)
	return srv, nil
}

// readOnlyTool applies the annotations shared by every retrieval tool.
func readOnlyTool(name, title, description string, opts ...mcp.ToolOption) mcp.Tool {
	base := []mcp.ToolOption{
		mcp.WithDescription(description),
		mcp.WithTitleAnnotation(title),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	}
	return mcp.NewTool(name, append(base, opts...)...)
}

// symbolTypeNames renders the closed enumeration for a JSON schema enum.
func symbolTypeNames() []string {
	types := model.SymbolTypes()
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

func searchTool() mcp.Tool {
	return readOnlyTool("global_codebase_search", "Global codebase search",
		"Look up a function or method by name across every indexed C and Go repository. "+
			"Returns the record with repo_name, file_path, language ('c' or 'go'), symbol_type, "+
			"line range, the boilerplate-free documentation block and the exact source body. "+
			"Use the language field to pick the correct markdown fence when quoting the code.",
		mcp.WithString("target_function_name",
			mcp.Required(),
			mcp.Description("Exact function or method name, e.g. 'process_payment' or 'Server.Handle'."),
		),
	)
}

func lookupSymbolTool() mcp.Tool {
	return readOnlyTool("lookup_symbol", "Look up any symbol",
		"Constant-time retrieval of any indexed declaration - function, method, struct, interface, "+
			"macro, macro_function, enum, typedef, constant or type_alias - across every repository. "+
			"The name may be an exact identifier or a prefix. Returns a JSON array of records ordered "+
			"with exact matches first.",
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Exact symbol name or identifier prefix, e.g. 'fsal_obj_handle' or 'ERR_FSAL'."),
		),
		mcp.WithString("repo_name",
			mcp.Description("Optional repository directory name to restrict the search."),
		),
		mcp.WithString("symbol_type",
			mcp.Description("Optional declaration kind filter."),
			mcp.Enum(symbolTypeNames()...),
		),
	)
}

func typeDefinitionTool() mcp.Tool {
	return readOnlyTool("get_type_definition", "Get a type definition",
		"Retrieve the exact declaration layout of a struct, union, interface, enum, typedef or type "+
			"alias, including field comments, embedded types and Go struct tags. Use this before "+
			"reasoning about memory layout, serialization or v-tables.",
		mcp.WithString("type_name",
			mcp.Required(),
			mcp.Description("Name of the struct, union, interface or typedef, e.g. 'fsal_obj_handle' or 'SessionManager'."),
		),
		mcp.WithString("repo_name",
			mcp.Description("Optional repository directory name to restrict the search."),
		),
	)
}

func macroOrConstTool() mcp.Tool {
	return readOnlyTool("lookup_macro_or_const", "Resolve a macro or constant",
		"Resolve a C preprocessor #define, an enum constant or a Go const declaration to its "+
			"replacement value, definition location and surrounding documentation. Grouped iota and "+
			"enum blocks are returned whole so neighbouring values stay visible.",
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Identifier name, e.g. 'ERR_FSAL_NO_QUOTA' or 'StatusPending'."),
		),
		mcp.WithString("repo_name",
			mcp.Description("Optional repository directory name to restrict the search."),
		),
	)
}

func readSnippetTool() mcp.Tool {
	return readOnlyTool("read_code_snippet", "Read a code snippet",
		fmt.Sprintf("Read a contiguous, line-numbered range of a repository file to verify the context "+
			"around a symbol returned by the other tools. At most %d lines are returned per call.",
			indexer.MaxSnippetLines),
		mcp.WithString("repo_name",
			mcp.Required(),
			mcp.Description("Repository directory name, e.g. 'nfs-ganesha'."),
		),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("Repository-relative file path exactly as reported in a record's file_path field."),
		),
		mcp.WithNumber("start_line",
			mcp.Required(),
			mcp.Min(1),
			mcp.Description("First line to return (1-based, inclusive)."),
		),
		mcp.WithNumber("end_line",
			mcp.Required(),
			mcp.Min(1),
			mcp.Description(fmt.Sprintf("Last line to return (inclusive). Capped at start_line + %d.", indexer.MaxSnippetLines-1)),
		),
	)
}

func capabilitiesTool() mcp.Tool {
	return readOnlyTool("list_repo_capabilities", "List repository capabilities",
		"Describe what a microservice does and which engineering language stack it is built on. "+
			"Combines the curated repository capability matrix with the structural footprint "+
			"observed in the local AST cache.",
		mcp.WithString("repo_name",
			mcp.Required(),
			mcp.Description("Repository directory name, e.g. 'auth-service-go'."),
		),
	)
}

// query is the normalised input shared by the retrieval handlers.
type query struct {
	name  string
	repo  string
	types []model.SymbolType
	exact bool
}

// resolve performs the O(1) cache lookup and, when nothing is cached, falls back
// to a time-bounded live Filter-Then-Parse scan of the workspace.
func (d Deps) resolve(ctx context.Context, q query) ([]model.SymbolRecord, error) {
	records, err := d.Cache.Lookup(q.name, store.Filter{
		Repo:      q.repo,
		Types:     q.types,
		ExactOnly: q.exact,
		Limit:     maxResults,
	})
	if err != nil {
		return nil, err
	}
	if len(records) > 0 {
		d.Log.Debug("cache hit", "symbol", q.name, "matches", len(records))
		return records, nil
	}
	if !d.LiveScan {
		return nil, nil
	}

	scanCtx, cancel := context.WithTimeout(ctx, liveScanTimeout)
	defer cancel()

	d.Log.Info("cache miss, running live workspace scan", "symbol", q.name)
	live, err := d.Scanner.SearchLive(scanCtx, q.name, q.types)
	if err != nil {
		return nil, err
	}
	if q.repo == "" {
		return live, nil
	}
	filtered := live[:0]
	for _, rec := range live {
		if rec.RepoName == q.repo {
			filtered = append(filtered, rec)
		}
	}
	return filtered, nil
}

// handleGlobalSearch keeps the Phase 1 tool contract: one callable symbol in,
// one record out. The payload is a superset of the original schema.
func (d Deps) handleGlobalSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, ok := requiredString(req, "target_function_name")
	if !ok {
		return mcp.NewToolResultError("target_function_name is required and must be a non-empty string"), nil
	}

	records, err := d.resolve(ctx, query{name: target, types: model.CallableTypes, exact: true})
	if err != nil {
		d.Log.Error("global search failed", "function", target, "error", err)
		return mcp.NewToolResultErrorFromErr("global codebase search failed", err), nil
	}
	switch len(records) {
	case 0:
		return mcp.NewToolResultError(notFoundMessage("function", target)), nil
	case 1:
		return renderJSON(records[0])
	default:
		return renderJSON(records)
	}
}

// handleLookupSymbol implements the generalised lookup_symbol tool.
func (d Deps) handleLookupSymbol(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, ok := requiredString(req, "name")
	if !ok {
		return mcp.NewToolResultError("name is required and must be a non-empty string"), nil
	}
	repo, errResult := optionalRepo(req)
	if errResult != nil {
		return errResult, nil
	}

	var types []model.SymbolType
	if raw := strings.TrimSpace(req.GetString("symbol_type", "")); raw != "" {
		t, err := model.ParseSymbolType(raw)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("invalid symbol_type", err), nil
		}
		types = []model.SymbolType{t}
	}

	records, err := d.resolve(ctx, query{name: name, repo: repo, types: types})
	if err != nil {
		d.Log.Error("symbol lookup failed", "symbol", name, "error", err)
		return mcp.NewToolResultErrorFromErr("symbol lookup failed", err), nil
	}
	if len(records) == 0 {
		return mcp.NewToolResultError(notFoundMessage("symbol", name)), nil
	}
	return renderJSON(records)
}

// handleTypeDefinition implements get_type_definition.
func (d Deps) handleTypeDefinition(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, ok := requiredString(req, "type_name")
	if !ok {
		return mcp.NewToolResultError("type_name is required and must be a non-empty string"), nil
	}
	repo, errResult := optionalRepo(req)
	if errResult != nil {
		return errResult, nil
	}

	records, err := d.resolve(ctx, query{
		name:  name,
		repo:  repo,
		types: model.TypeDefinitionTypes,
		exact: true,
	})
	if err != nil {
		d.Log.Error("type lookup failed", "type", name, "error", err)
		return mcp.NewToolResultErrorFromErr("type definition lookup failed", err), nil
	}
	if len(records) == 0 {
		return mcp.NewToolResultError(notFoundMessage("type", name)), nil
	}
	return renderJSON(records)
}

// handleMacroOrConst implements lookup_macro_or_const.
func (d Deps) handleMacroOrConst(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, ok := requiredString(req, "name")
	if !ok {
		return mcp.NewToolResultError("name is required and must be a non-empty string"), nil
	}
	repo, errResult := optionalRepo(req)
	if errResult != nil {
		return errResult, nil
	}

	records, err := d.resolve(ctx, query{
		name:  name,
		repo:  repo,
		types: model.MacroConstTypes,
		exact: true,
	})
	if err != nil {
		d.Log.Error("macro lookup failed", "name", name, "error", err)
		return mcp.NewToolResultErrorFromErr("macro or constant lookup failed", err), nil
	}
	if len(records) == 0 {
		return mcp.NewToolResultError(notFoundMessage("macro or constant", name)), nil
	}
	return renderJSON(records)
}

// handleReadSnippet implements read_code_snippet.
func (d Deps) handleReadSnippet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo, ok := requiredString(req, "repo_name")
	if !ok {
		return mcp.NewToolResultError("repo_name is required and must be a non-empty string"), nil
	}
	if err := indexer.ValidateRepoName(repo); err != nil {
		return mcp.NewToolResultErrorFromErr("invalid repo_name", err), nil
	}
	path, ok := requiredString(req, "file_path")
	if !ok {
		return mcp.NewToolResultError("file_path is required and must be a non-empty string"), nil
	}
	start, err := req.RequireInt("start_line")
	if err != nil {
		return mcp.NewToolResultError("start_line is required and must be an integer"), nil
	}
	end, err := req.RequireInt("end_line")
	if err != nil {
		return mcp.NewToolResultError("end_line is required and must be an integer"), nil
	}

	snippet, err := d.Scanner.ReadSnippet(repo, path, start, end)
	if err != nil {
		d.Log.Warn("snippet read failed", "repo", repo, "file", path, "error", err)
		return mcp.NewToolResultErrorFromErr("could not read the requested snippet", err), nil
	}
	return mcp.NewToolResultText(snippet), nil
}

// handleListCapabilities renders the capability briefing for one repository.
func (d Deps) handleListCapabilities(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo, ok := requiredString(req, "repo_name")
	if !ok {
		return mcp.NewToolResultError("repo_name is required and must be a non-empty string"), nil
	}
	if err := indexer.ValidateRepoName(repo); err != nil {
		return mcp.NewToolResultErrorFromErr("invalid repo_name", err), nil
	}

	stats, err := d.Cache.Stats()
	if err != nil {
		d.Log.Error("cache statistics failed", "repo", repo, "error", err)
		return mcp.NewToolResultErrorFromErr("cache statistics failed", err), nil
	}

	stat, indexed := stats[repo]
	obs := capabilities.Observation{
		Indexed:       indexed,
		SymbolCount:   stat.Symbols,
		FunctionCount: stat.Functions,
		FileCount:     len(stat.Files),
		Languages:     stat.Languages,
		SymbolTypes:   stat.SymbolTypes,
		Samples:       stat.Samples,
	}

	if _, curated := d.Matrix.Lookup(repo); !curated && !indexed {
		known, listErr := d.Scanner.ListRepos()
		if listErr == nil && len(known) > 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"Unknown repository %q. Known repositories: %s.", repo, strings.Join(known, ", "),
			)), nil
		}
	}

	return mcp.NewToolResultText(d.Matrix.Describe(repo, obs)), nil
}

// requiredString reads and trims a mandatory string argument.
func requiredString(req mcp.CallToolRequest, key string) (string, bool) {
	raw, err := req.RequireString(key)
	if err != nil {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	return raw, raw != ""
}

// optionalRepo validates the optional repo_name filter shared by several tools.
func optionalRepo(req mcp.CallToolRequest) (string, *mcp.CallToolResult) {
	repo := strings.TrimSpace(req.GetString("repo_name", ""))
	if repo == "" {
		return "", nil
	}
	if err := indexer.ValidateRepoName(repo); err != nil {
		return "", mcp.NewToolResultErrorFromErr("invalid repo_name", err)
	}
	return repo, nil
}

// renderJSON serialises a payload exactly as specified by the cache schema.
func renderJSON(payload any) (*mcp.CallToolResult, error) {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to serialise the cached record", err), nil
	}
	return mcp.NewToolResultText(string(encoded)), nil
}

func notFoundMessage(kind, target string) string {
	return fmt.Sprintf(
		"No indexed %s named %q was found in any repository. "+
			"Verify the exact identifier spelling, or re-index the owning repository via "+
			"POST /api/v1/<repo_name>/update before retrying.", kind, target,
	)
}
