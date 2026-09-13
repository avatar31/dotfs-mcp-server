// Command dotfs-mcp-server is a Model Context Protocol server that exposes
// structural, AST-level knowledge of a multi-repository C and Go workspace to
// an LLM client over stdio, while serving a management REST API for on-demand
// re-indexing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	mcpsrv "github.com/mark3labs/mcp-go/server"
	"go.uber.org/zap"

	"github.com/avatar31/dotfs-mcp-server/internal/ast/indexer"
	"github.com/avatar31/dotfs-mcp-server/internal/ast/parser"
	"github.com/avatar31/dotfs-mcp-server/internal/ast/store"
	"github.com/avatar31/dotfs-mcp-server/internal/capabilities"
	"github.com/avatar31/dotfs-mcp-server/internal/config"
	"github.com/avatar31/dotfs-mcp-server/internal/httpapi"
	"github.com/avatar31/dotfs-mcp-server/internal/logger"
	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
	"github.com/avatar31/dotfs-mcp-server/internal/mcpserver"
	"github.com/avatar31/dotfs-mcp-server/internal/utils"
	"github.com/avatar31/dotfs-mcp-server/internal/xref"
)

func main() {
	workspaceDirPtr := flag.String("workspace", "", "Path to the workspace root directory")
	setupPtr := flag.Bool("setup", false, "Run the initial workspace setup")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}

	if workspaceDirPtr != nil && *workspaceDirPtr != "" {
		cfg.WorkspaceRoot = *workspaceDirPtr
	}

	if cfg.WorkspaceRoot == "" {
		fmt.Fprintln(os.Stderr, "fatal: workspace root directory must be specified with -workspace")
		os.Exit(1)
	}

	if setupPtr != nil && *setupPtr {
		err := preconfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(cfg); err != nil {
		// stdout belongs to the MCP transport, so failures go to stderr only.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	log, err := logger.NewZapLogger("dotfs-mcp-server", cfg.LogPath)
	if err != nil {
		return err
	}
	defer log.Sync()

	log.Info("dotfs-mcp-server starting",
		zap.String("version", cfg.ServerVersion),
		zap.String("workspace_root", cfg.WorkspaceRoot),
		zap.String("cache_dir", cfg.CacheDir),
		zap.Bool("http_enabled", cfg.EnableHTTP),
	)

	// Signal-aware root context shared by the stdio transport, the HTTP API and
	// every background indexing worker.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cache, err := store.Open(cfg.CacheDir, log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := cache.Close(); cerr != nil {
			log.Error("failed to close cache", zap.Error(cerr))
		}
	}()

	registry := parser.NewDefaultRegistry()
	log.Debug("parser engines registered", zap.Strings("extensions", registry.Extensions()))

	idx, err := indexer.New(cache, registry, log, indexer.Options{
		WorkspaceRoot: cfg.WorkspaceRoot,
		MaxFileSize:   cfg.MaxFileSize,
		SkipDirs:      cfg.SkipDirs,
	})
	if err != nil {
		return err
	}

	matrix, err := capabilities.Load()
	if err != nil {
		return err
	}

	if cfg.IndexOnStart {
		// Indexing runs asynchronously so the MCP handshake is never delayed by
		// a cold cache on a large workspace.
		go func() {
			summaries, err := idx.IndexAll(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Error("initial workspace index failed", zap.Error(err))
				return
			}
			log.Info("initial workspace index complete", zap.Int("repositories", len(summaries)))
		}()
	}

	gcDone := startValueLogGC(ctx, cache, log, cfg.GCInterval)

	var api *httpapi.Server
	apiErrCh := make(chan error, 1)
	if cfg.EnableHTTP {
		api, err = httpapi.New(ctx, httpapi.Config{
			Addr:          cfg.HTTPAddr,
			APIToken:      cfg.APIToken,
			WorkspaceRoot: cfg.WorkspaceRoot,
		}, idx, log.With(zap.String("component", "httpapi")))
		if err != nil {
			return err
		}
		go func() { apiErrCh <- api.ListenAndServe() }()
	}

	// The language-server pool is created eagerly but spawns nothing
	// until a relational tool is actually called.
	crossRef, closeLSP, err := startCrossReference(cfg, log)
	if err != nil {
		return err
	}
	defer closeLSP()

	mcpServer, err := mcpserver.New(mcpserver.Deps{
		Cache:    cache,
		Scanner:  idx,
		Matrix:   matrix,
		Log:      log.With(zap.String("component", "mcp")),
		Name:     cfg.ServerName,
		Version:  cfg.ServerVersion,
		LiveScan: true,
		XRef:     crossRef,
	})
	if err != nil {
		return err
	}

	httpSrv := mcpsrv.NewStreamableHTTPServer(mcpServer)

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- httpSrv.Start(":9701") }()

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
		deadlineCtx, cancelShutdown := context.WithTimeout(ctx, 5*time.Second)
		defer cancelShutdown()
		if err := httpSrv.Shutdown(deadlineCtx); err != nil {
			log.Error("Failed to shutdown mcp server", zap.Error(err))
		}
	case err := <-serveErrCh:
		if err != nil {
			runErr = err
		}
	case err := <-apiErrCh:
		if err != nil {
			runErr = err
			deadlineCtx, cancelShutdown := context.WithTimeout(ctx, 5*time.Second)
			defer cancelShutdown()
			if err := httpSrv.Shutdown(deadlineCtx); err != nil {
				log.Error("Failed to shutdown mcp server", zap.Error(err))
			}
		}
	}

	stop() // cancel the root context, which stops all background workers
	if api != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := api.Shutdown(shutdownCtx); err != nil {
			log.Error("management API shutdown failed", zap.Error(err))
		}
	}
	<-gcDone

	log.Info("dotfs-mcp-server stopped")
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

	_, err = exec.LookPath(cfg.LSPConfig.ClangdPath)
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

	err = initGopls(cfg)
	if err != nil {
		return err
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

func initGopls(cfg *config.Config) error {
	_, err := exec.LookPath(cfg.LSPConfig.GoplsPath)
	if err != nil {
		return err
	}

	err = os.Remove(fmt.Sprintf("%s/go.work", cfg.WorkspaceRoot))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Initialize a Go workspace in the root directory of the project to enable gopls to work with multiple modules.
	goWorkInitCmd := exec.Command("go", "work", "init", "./omashu", "./dotfs", "./halmidi")
	goWorkInitCmd.Dir = cfg.WorkspaceRoot
	err = goWorkInitCmd.Run()
	if err != nil {
		return err
	}

	mcpInstrCmd := exec.Command(cfg.LSPConfig.GoplsPath, "mcp", "-instructions")
	stdout, err := mcpInstrCmd.Output()
	if err != nil {
		return err
	}

	err = os.WriteFile(fmt.Sprintf("%s/%s", cfg.WorkspaceRoot, utils.GOPLS_INSTRUCTION_FILE), stdout, 0644)
	if err != nil {
		return err
	}

	return nil
}

func startCrossReference(cfg *config.Config, log *zap.Logger) (mcpserver.CrossReference, func(), error) {
	manager := lsp.NewManager(cfg, log.With(zap.String("component", "lsp")))
	service, err := xref.New(xref.FromManager(manager), cfg.WorkspaceRoot, log.With(zap.String("component", "xref")))
	if err != nil {
		return nil, nil, err
	}

	log.Info("cross-reference engine ready",
		zap.String("gopls", cfg.LSPConfig.GoplsPath),
		zap.String("clangd", cfg.LSPConfig.ClangdPath),
		zap.Duration("request_timeout", cfg.LSPConfig.RequestTimeout),
	)

	shutdown := func() {
		// Detached from the root context, which is already cancelled by now:
		// the daemons still need a window to exit before they are killed.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			log.Error("language server shutdown failed", zap.Error(err))
		}
	}
	return service, shutdown, nil
}

// startValueLogGC periodically reclaims BadgerDB value-log space and returns a
// channel closed once the collector has stopped.
func startValueLogGC(ctx context.Context, cache *store.Store, logger *zap.Logger,
	every time.Duration) <-chan struct{} {
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
					logger.Warn("cache garbage collection failed", zap.Error(err))
				}
			}
		}
	}()
	return done
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
