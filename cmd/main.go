// Command dotfs-mcp-server is a Model Context Protocol server that exposes
// structural, AST-level knowledge of a multi-repository C and Go workspace to
// an LLM client over stdio, while serving a management REST API for on-demand
// re-indexing.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/capabilities"
	"github.com/avatar31/dotfs-mcp-server/internal/config"
	"github.com/avatar31/dotfs-mcp-server/internal/indexer"
	"github.com/avatar31/dotfs-mcp-server/internal/parser"
	"github.com/avatar31/dotfs-mcp-server/internal/store"
)

func main() {
	if err := run(); err != nil {
		// stdout belongs to the MCP transport, so failures go to stderr only.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("starting dotfs-mcp-server",
		"version", cfg.ServerVersion,
		"workspace_root", cfg.WorkspaceRoot,
		"cache_dir", cfg.CacheDir,
		"http_enabled", cfg.EnableHTTP,
	)

	// Signal-aware root context shared by the stdio transport, the HTTP API and
	// every background indexing worker.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cache, err := store.Open(cfg.CacheDir, logger)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := cache.Close(); cerr != nil {
			logger.Error("failed to close cache", "error", cerr)
		}
	}()

	registry := parser.NewDefaultRegistry()
	logger.Debug("parser engines registered", "extensions", registry.Extensions())

	idx, err := indexer.New(cache, registry, logger, indexer.Options{
		WorkspaceRoot: cfg.WorkspaceRoot,
		MaxFileSize:   cfg.MaxFileSize,
		SkipDirs:      cfg.SkipDirs,
	})
	if err != nil {
		return err
	}

	_, err = capabilities.Load(cfg.CapabilitiesFile)
	if err != nil {
		return err
	}

	if cfg.IndexOnStart {
		// Indexing runs asynchronously so the MCP handshake is never delayed by
		// a cold cache on a large workspace.
		go func() {
			summaries, err := idx.IndexAll(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("initial workspace index failed", "error", err)
				return
			}
			logger.Info("initial workspace index complete", "repositories", len(summaries))
		}()
	}

	gcDone := startValueLogGC(ctx, cache, logger, cfg.GCInterval)

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	stop() // cancel the root context, which stops all background workers
	<-gcDone

	logger.Info("dotfs-mcp-server stopped")
	return runErr
}

// startValueLogGC periodically reclaims BadgerDB value-log space and returns a
// channel closed once the collector has stopped.
func startValueLogGC(ctx context.Context, cache *store.Store, logger *slog.Logger, every time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := cache.RunValueLogGC(0.7); err != nil {
					logger.Warn("cache garbage collection failed", "error", err)
				}
			}
		}
	}()
	return done
}

// newLogger builds the structured logger. It writes to stderr because stdout is
// reserved for the MCP JSON-RPC framing.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
