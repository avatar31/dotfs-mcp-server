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

// Cache is the read/lookup surface required from the storage layer.
type Cache interface {
	Get(name string) (model.FunctionRecord, error)
	Stats() (map[string]store.RepoStat, error)
}

// LiveScanner is the fallback used when the cache misses.
type LiveScanner interface {
	SearchLive(ctx context.Context, target string) (model.FunctionRecord, bool, error)
	ListRepos() ([]string, error)
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
	srv.AddTool(capabilitiesTool(), deps.handleListCapabilities)
	return srv, nil
}

func searchTool() mcp.Tool {
	return mcp.NewTool("global_codebase_search",
		mcp.WithDescription(
			"Look up a function or method by name across every indexed C and Go repository. "+
				"Returns a JSON document with repo_name, file_path, language ('c' or 'go'), "+
				"the boilerplate-free documentation block and the exact source body. "+
				"Use the language field to pick the correct markdown fence when quoting the code.",
		),
		mcp.WithTitleAnnotation("Global codebase search"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("target_function_name",
			mcp.Required(),
			mcp.Description("Exact function or method name, e.g. 'process_payment' or 'Server.Handle'."),
		),
	)
}

func capabilitiesTool() mcp.Tool {
	return mcp.NewTool("list_repo_capabilities",
		mcp.WithDescription(
			"Describe what a microservice does and which engineering language stack it is built on. "+
				"Combines the curated repository capability matrix with the structural footprint "+
				"observed in the local AST cache.",
		),
		mcp.WithTitleAnnotation("List repository capabilities"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("repo_name",
			mcp.Required(),
			mcp.Description("Repository directory name, e.g. 'auth-service-go'."),
		),
	)
}

// handleGlobalSearch performs the O(1) BadgerDB lookup and, on a miss, falls
// back to a live Filter-Then-Parse scan of the workspace.
func (d Deps) handleGlobalSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	target, err := req.RequireString("target_function_name")
	if err != nil {
		return mcp.NewToolResultError("target_function_name is required and must be a string"), nil
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return mcp.NewToolResultError("target_function_name must not be empty"), nil
	}

	rec, err := d.Cache.Get(target)
	switch {
	case err == nil:
		d.Log.Debug("cache hit", "function", target, "repo", rec.RepoName)
		return renderRecord(rec)
	case !errors.Is(err, store.ErrNotFound):
		d.Log.Error("cache lookup failed", "function", target, "error", err)
		return mcp.NewToolResultErrorFromErr("cache lookup failed", err), nil
	}

	if !d.LiveScan {
		return mcp.NewToolResultError(notFoundMessage(target)), nil
	}

	scanCtx, cancel := context.WithTimeout(ctx, liveScanTimeout)
	defer cancel()

	d.Log.Info("cache miss, running live workspace scan", "function", target)
	rec, found, err := d.Scanner.SearchLive(scanCtx, target)
	switch {
	case err != nil:
		d.Log.Error("live scan failed", "function", target, "error", err)
		return mcp.NewToolResultErrorFromErr("live workspace scan failed", err), nil
	case !found:
		return mcp.NewToolResultError(notFoundMessage(target)), nil
	}
	return renderRecord(rec)
}

// handleListCapabilities renders the capability briefing for one repository.
func (d Deps) handleListCapabilities(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo, err := req.RequireString("repo_name")
	if err != nil {
		return mcp.NewToolResultError("repo_name is required and must be a string"), nil
	}
	repo = strings.TrimSpace(repo)
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
		FunctionCount: stat.Functions,
		FileCount:     len(stat.Files),
		Languages:     stat.Languages,
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

// renderRecord serialises the record exactly as specified by the cache schema.
func renderRecord(rec model.FunctionRecord) (*mcp.CallToolResult, error) {
	payload, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to serialise the cached record", err), nil
	}
	return mcp.NewToolResultText(string(payload)), nil
}

func notFoundMessage(target string) string {
	return fmt.Sprintf(
		"No indexed function named %q was found in any repository. "+
			"Verify the exact symbol spelling, or re-index the owning repository via "+
			"POST /api/v1/<repo_name>/update before retrying.", target,
	)
}
