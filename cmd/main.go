// Command dotfs-mcp-server is a Model Context Protocol server that exposes
// structural, AST-level knowledge of a multi-repository C and Go workspace to
// an LLM client over stdio, while serving a management REST API for on-demand
// re-indexing.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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

	_, err = indexer.New(cache, registry, logger, indexer.Options{
		WorkspaceRoot: cfg.WorkspaceRoot,
		MaxFileSize:   cfg.MaxFileSize,
		SkipDirs:      cfg.SkipDirs,
	})
	if err != nil {
		return err
	}

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	stop() // cancel the root context, which stops all background workers

	logger.Info("dotfs-mcp-server stopped")
	return runErr
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
