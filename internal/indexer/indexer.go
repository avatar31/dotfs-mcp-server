// Package indexer implements the two-phase "Filter-Then-Parse" algorithm used
// to populate and refresh the BadgerDB cache across a multi-repository,
// multi-language workspace.
//
//	Phase 1  language-agnostic byte scan (bytes.Contains) - cheap rejection
//	Phase 2  extension-routed AST extraction              - precise isolation
package indexer

import (
	"errors"
	"log/slog"
	"runtime"

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
