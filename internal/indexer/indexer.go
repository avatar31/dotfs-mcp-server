// Package indexer implements the two-phase "Filter-Then-Parse" algorithm used
// to populate and refresh the BadgerDB cache across a multi-repository,
// multi-language workspace.
//
//	Phase 1  language-agnostic byte scan (bytes.Contains) - cheap rejection
//	Phase 2  extension-routed AST extraction              - precise isolation
package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
	"github.com/avatar31/dotfs-mcp-server/internal/parser"
	"github.com/avatar31/dotfs-mcp-server/internal/store"
)

// Options tunes a workspace crawl.
type Options struct {
	WorkspaceRoot string
	MaxFileSize   int64
	SkipDirs      []string
	Workers       int
}

// Summary is the machine-readable outcome of an indexing cycle.
type Summary struct {
	Repo           string `json:"repo"`
	FilesScanned   int    `json:"files_scanned"`
	FilesFiltered  int    `json:"files_filtered_out"`
	FilesParsed    int    `json:"files_parsed"`
	FunctionsFound int    `json:"functions_found"`
	RecordsWritten int    `json:"records_written"`
	RecordsPruned  int    `json:"records_pruned"`
	ParseErrors    int    `json:"parse_errors"`
	DurationMS     int64  `json:"duration_ms"`
}

// Indexer crawls repositories and keeps the cache in sync.
type Indexer struct {
	store    *store.Store
	registry *parser.Registry
	log      *slog.Logger
	opts     Options
	skipDirs map[string]struct{}
}

// New builds an Indexer. Workers defaults to the CPU count.
func New(st *store.Store, registry *parser.Registry, log *slog.Logger, opts Options) (*Indexer, error) {
	if st == nil || registry == nil {
		return nil, errors.New("indexer requires a store and a parser registry")
	}
	if opts.WorkspaceRoot == "" {
		return nil, errors.New("indexer requires a workspace root")
	}
	if opts.MaxFileSize <= 0 {
		return nil, errors.New("indexer requires a positive max file size")
	}
	if opts.Workers <= 0 {
		opts.Workers = runtime.NumCPU()
	}

	skip := make(map[string]struct{}, len(opts.SkipDirs))
	for _, d := range opts.SkipDirs {
		skip[d] = struct{}{}
	}
	return &Indexer{store: st, registry: registry, log: log, opts: opts, skipDirs: skip}, nil
}

// ListRepos returns every addressable repository directory under the workspace.
func (ix *Indexer) ListRepos() ([]string, error) {
	entries, err := os.ReadDir(ix.opts.WorkspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("read workspace root %q: %w", ix.opts.WorkspaceRoot, err)
	}

	var repos []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		if _, skipped := ix.skipDirs[name]; skipped {
			continue
		}
		if ValidateRepoName(name) != nil {
			ix.log.Debug("skipping non-addressable workspace entry", "entry", name)
			continue
		}
		repos = append(repos, name)
	}
	sort.Strings(repos)
	return repos, nil
}

// IndexAll re-indexes every repository in the workspace sequentially so the
// host CPU and SSD are never saturated by parallel repository crawls.
func (ix *Indexer) IndexAll(ctx context.Context) ([]Summary, error) {
	repos, err := ix.ListRepos()
	if err != nil {
		return nil, err
	}

	summaries := make([]Summary, 0, len(repos))
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return summaries, err
		}
		summary, err := ix.IndexRepo(ctx, repo)
		if err != nil {
			// One broken repository must not abort the whole workspace crawl.
			ix.log.Error("repository indexing failed", "repo", repo, "error", err)
			continue
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// parseResult carries AST output from a worker to the single cache writer.
type parseResult struct {
	relPath   string
	functions []parser.Function
	err       error
}

// IndexRepo runs the full Filter-Then-Parse cycle for one repository and prunes
// cache records whose functions no longer exist in the source tree.
func (ix *Indexer) IndexRepo(ctx context.Context, repo string) (Summary, error) {
	started := time.Now()

	repoPath, err := SafeRepoPath(ix.opts.WorkspaceRoot, repo)
	if err != nil {
		return Summary{}, err
	}

	files, scanned, err := ix.collectFiles(ctx, repoPath)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{Repo: repo, FilesScanned: scanned}

	results := make(chan parseResult, ix.opts.Workers)
	jobs := make(chan string)

	// 1 Producer goroutine feeds the jobs channel with every parseable file path.
	// N Consumer goroutines read jobs and send parse results to the results channel.
	var wg sync.WaitGroup
	for i := 0; i < ix.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				results <- ix.parseFile(ctx, repoPath, path, "")
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, path := range files {
			select {
			case <-ctx.Done():
				return
			case jobs <- path:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	// A single writer goroutine (this one) keeps BadgerDB transactions free of
	// write-write conflicts while readers from the LLM client stay unblocked.
	seen := make(map[string]struct{})
	for res := range results {
		switch {
		case res.err != nil:
			summary.ParseErrors++
			ix.log.Warn("parse failure", "repo", repo, "file", res.relPath, "error", res.err)
			continue
		case res.functions == nil:
			summary.FilesFiltered++
			continue
		}

		summary.FilesParsed++
		summary.FunctionsFound += len(res.functions)

		for _, fn := range res.functions {
			rec := model.FunctionRecord{
				RepoName:      repo,
				FilePath:      filepath.ToSlash(filepath.Join(repo, res.relPath)),
				Language:      fn.Language,
				Documentation: fn.Documentation,
				SourceCode:    fn.SourceCode,
			}
			for _, key := range append([]string{fn.Name}, fn.Aliases...) {
				written, err := ix.store.Put(key, rec)
				if err != nil {
					summary.ParseErrors++
					ix.log.Error("cache write failed", "repo", repo, "function", key, "error", err)
					continue
				}
				seen[key] = struct{}{}
				if written {
					summary.RecordsWritten++
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return summary, err
	}

	pruned, err := ix.pruneStale(repo, seen)
	if err != nil {
		return summary, err
	}
	summary.RecordsPruned = pruned
	summary.DurationMS = time.Since(started).Milliseconds()

	ix.log.Info("repository indexed",
		"repo", repo,
		"files_scanned", summary.FilesScanned,
		"files_parsed", summary.FilesParsed,
		"functions", summary.FunctionsFound,
		"written", summary.RecordsWritten,
		"pruned", summary.RecordsPruned,
		"errors", summary.ParseErrors,
		"duration_ms", summary.DurationMS,
	)
	return summary, nil
}

// SearchLive is the real-time fallback used when the cache misses: it applies
// Phase 1 (bytes.Contains) across the workspace and only pays for Phase 2 on
// files that literally contain the token. Matches are written back to cache.
func (ix *Indexer) SearchLive(ctx context.Context, target string) (model.FunctionRecord, bool, error) {
	repos, err := ix.ListRepos()
	if err != nil {
		return model.FunctionRecord{}, false, err
	}

	for _, repo := range repos {
		repoPath, err := SafeRepoPath(ix.opts.WorkspaceRoot, repo)
		if err != nil {
			ix.log.Warn("skipping unreadable repository", "repo", repo, "error", err)
			continue
		}
		files, _, err := ix.collectFiles(ctx, repoPath)
		if err != nil {
			return model.FunctionRecord{}, false, err
		}

		for _, path := range files {
			if err := ctx.Err(); err != nil {
				return model.FunctionRecord{}, false, err
			}

			res := ix.parseFile(ctx, repoPath, path, target)
			if res.err != nil {
				ix.log.Debug("live scan parse failure", "file", res.relPath, "error", res.err)
				continue
			}

			for _, fn := range res.functions {
				if !matchesName(fn, target) {
					continue
				}
				rec := model.FunctionRecord{
					RepoName:      repo,
					FilePath:      filepath.ToSlash(filepath.Join(repo, res.relPath)),
					Language:      fn.Language,
					Documentation: fn.Documentation,
					SourceCode:    fn.SourceCode,
				}
				if _, err := ix.store.Put(target, rec); err != nil {
					ix.log.Error("failed to cache live scan hit", "function", target, "error", err)
				}
				return rec, true, nil
			}
		}
	}
	return model.FunctionRecord{}, false, nil
}

func matchesName(fn parser.Function, target string) bool {
	if fn.Name == target {
		return true
	}
	for _, alias := range fn.Aliases {
		if alias == target {
			return true
		}
	}
	return false
}

// parseFile executes Phase 1 then Phase 2 for a single file. A nil function
// slice with a nil error means the file was rejected by the Phase 1 filter.
func (ix *Indexer) parseFile(ctx context.Context, repoPath, path, target string) parseResult {
	rel, err := filepath.Rel(repoPath, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	engine, ok := ix.registry.For(path)
	if !ok {
		return parseResult{relPath: rel}
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return parseResult{relPath: rel, err: fmt.Errorf("read %s: %w", rel, err)}
	}

	// ---- Phase 1: language-agnostic byte scan -------------------------------
	if target != "" {
		if !bytes.Contains(src, []byte(target)) {
			return parseResult{relPath: rel}
		}
	} else if !engine.Prefilter(src) {
		return parseResult{relPath: rel}
	}

	// ---- Phase 2: routing-aware AST extraction ------------------------------
	functions, err := engine.Parse(ctx, path, src)
	if err != nil {
		return parseResult{relPath: rel, err: err}
	}
	if functions == nil {
		// Distinguish "parsed, nothing found" from "filtered out" for the caller.
		functions = []parser.Function{}
	}
	return parseResult{relPath: rel, functions: functions}
}

// collectFiles walks a repository and returns every parseable file path
// for all supported file extensions.
func (ix *Indexer) collectFiles(ctx context.Context, repoPath string) ([]string, int, error) {
	var files []string
	scanned := 0

	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			ix.log.Warn("walk error", "path", path, "error", err)
			return nil // keep crawling the rest of the tree
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		if d.IsDir() {
			if _, skipped := ix.skipDirs[d.Name()]; skipped && path != repoPath {
				return filepath.SkipDir
			}
			return nil
		}
		// Never follow symlinks: they can point outside the workspace root.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !ix.registry.Supports(path) {
			return nil
		}

		scanned++
		info, err := d.Info()
		if err != nil {
			ix.log.Warn("stat failed", "path", path, "error", err)
			return nil
		}
		if info.Size() > ix.opts.MaxFileSize {
			ix.log.Debug("skipping oversized file", "path", path, "size", info.Size())
			return nil
		}

		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, scanned, fmt.Errorf("walk repository %q: %w", repoPath, err)
	}
	return files, scanned, nil
}

// pruneStale evicts cache entries for functions that vanished from the repo.
func (ix *Indexer) pruneStale(repo string, seen map[string]struct{}) (int, error) {
	cached, err := ix.store.ListRepoFunctions(repo)
	if err != nil {
		return 0, err
	}

	var stale []string
	for _, name := range cached {
		if _, ok := seen[name]; !ok {
			stale = append(stale, name)
		}
	}
	return ix.store.PruneRepo(repo, stale)
}
