// Command dotfs-mcp-server is a Model Context Protocol server that exposes
// structural, AST-level knowledge of a multi-repository C and Go workspace to
// an LLM client over stdio, while serving a management REST API for on-demand
// re-indexing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	mcpsrv "github.com/mark3labs/mcp-go/server"

	"github.com/avatar31/dotfs-mcp-server/internal/ast/indexer"
	"github.com/avatar31/dotfs-mcp-server/internal/ast/parser"
	"github.com/avatar31/dotfs-mcp-server/internal/ast/store"
	"github.com/avatar31/dotfs-mcp-server/internal/capabilities"
	"github.com/avatar31/dotfs-mcp-server/internal/config"
	"github.com/avatar31/dotfs-mcp-server/internal/httpapi"
	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
	"github.com/avatar31/dotfs-mcp-server/internal/mcpserver"
	"github.com/avatar31/dotfs-mcp-server/internal/xref"
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

	err = preconfig(cfg)
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

	matrix, err := capabilities.Load("./capabilities/capabilities.json")
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

	var api *httpapi.Server
	apiErrCh := make(chan error, 1)
	if cfg.EnableHTTP {
		api, err = httpapi.New(ctx, httpapi.Config{
			Addr:          cfg.HTTPAddr,
			APIToken:      cfg.APIToken,
			WorkspaceRoot: cfg.WorkspaceRoot,
		}, idx, logger.With("component", "httpapi"))
		if err != nil {
			return err
		}
		go func() { apiErrCh <- api.ListenAndServe() }()
	}

	// The language-server pool is created eagerly but spawns nothing
	// until a relational tool is actually called.
	crossRef, closeLSP, err := startCrossReference(cfg, logger)
	if err != nil {
		return err
	}
	defer closeLSP()

	mcpServer, err := mcpserver.New(mcpserver.Deps{
		Cache:    cache,
		Scanner:  idx,
		Matrix:   matrix,
		Log:      logger.With("component", "mcp"),
		Name:     cfg.ServerName,
		Version:  cfg.ServerVersion,
		LiveScan: true,
		XRef:     crossRef,
	})
	if err != nil {
		return err
	}

	stdio := mcpsrv.NewStdioServer(mcpServer)
	stdio.SetErrorLogger(log.New(os.Stderr, "mcp-stdio ", log.LstdFlags))

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- stdio.Listen(ctx, os.Stdin, os.Stdout) }()

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serveErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			runErr = fmt.Errorf("mcp stdio transport: %w", err)
		}
	case err := <-apiErrCh:
		if err != nil {
			runErr = err
		}
	}

	stop() // cancel the root context, which stops all background workers
	if api != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := api.Shutdown(shutdownCtx); err != nil {
			logger.Error("management API shutdown failed", "error", err)
		}
	}
	<-gcDone

	logger.Info("dotfs-mcp-server stopped")
	return runErr
}

func preconfig(cfg *config.Config) error {
	info, err := os.Stat(fmt.Sprintf("%s/graphify-out", cfg.WorkspaceRoot))
	if err != nil {
		fmt.Printf("workspace root %q is not a valid graphify workspace: Generate graphify GRAPH_REPORT.md skill."+
			" Check prereq/graphify/README.md for more instructions", cfg.WorkspaceRoot)
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace root %q is not a valid graphify workspace", cfg.WorkspaceRoot)
	}

	if cfg.LSPConfig.Enabled {
		_, err := exec.LookPath(cfg.LSPConfig.ClangdPath)
		if err != nil {
			return err
		}

		// Run `cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON -S src/ -B build/` in nfs-ganesha dir
		_, err = os.Stat(fmt.Sprintf("%s/nfs-ganesha/build/compile_commands.json", cfg.WorkspaceRoot))
		if err != nil {
			return err
		}

		// _, err = os.Stat(fmt.Sprintf("%s/samba/build/compile_commands.json", cfg.WorkspaceRoot))
		// if err != nil {
		// 	return err
		// }

		_, err = exec.LookPath(cfg.LSPConfig.GoplsPath)
		if err != nil {
			return err
		}
	}

	err = cp("./prereqs/knowledge/dotfs_agent.md", fmt.Sprintf("%s/AGENT.md", cfg.WorkspaceRoot))
	if err != nil {
		return err
	}

	err = cp("./prereqs/knowledge/dotfs.workspace.md", fmt.Sprintf("%s/WORKSPACE.md", cfg.WorkspaceRoot))
	if err != nil {
		return err
	}

	copilotInstructions := `# Copilot Instructions
> **See also:** [AGENTS.md](../AGENTS.md) for more information on how to use the dotfs agent.
`

	err = createDirIfNotExists(fmt.Sprintf("%s/.github", cfg.WorkspaceRoot))
	if err != nil {
		return err
	}

	err = os.WriteFile(fmt.Sprintf("%s/.github/copilot-instructions.md", cfg.WorkspaceRoot), []byte(copilotInstructions), 0644)
	if err != nil {
		return err
	}

	return nil
}

func startCrossReference(cfg *config.Config, logger *slog.Logger) (mcpserver.CrossReference, func(), error) {
	if !cfg.LSPConfig.Enabled {
		logger.Info("cross-reference engine disabled", "reason", "DOTFS_LSP_ENABLED=false")
		return nil, func() {}, nil
	}

	manager := lsp.NewManager(cfg, logger.With("component", "lsp"))
	service, err := xref.New(xref.FromManager(manager), cfg.WorkspaceRoot, logger.With("component", "xref"))
	if err != nil {
		return nil, nil, err
	}

	logger.Info("cross-reference engine ready",
		"gopls", cfg.LSPConfig.GoplsPath, "clangd", cfg.LSPConfig.ClangdPath, "request_timeout", cfg.LSPConfig.RequestTimeout)

	shutdown := func() {
		// Detached from the root context, which is already cancelled by now:
		// the daemons still need a window to exit before they are killed.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			logger.Error("language server shutdown failed", "error", err)
		}
	}
	return service, shutdown, nil
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

func cp(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	return destination.Sync()
}

func createDirIfNotExists(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", path, err)
		}
	}
	return nil
}
