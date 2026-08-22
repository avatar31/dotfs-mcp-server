// Package httpapi implements the management REST API used by operators and CI
// webhooks to trigger repository-scoped re-indexing without restarting the
// MCP server.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/indexer"
	"github.com/avatar31/dotfs-mcp-server/internal/utils"
)

// jobTimeout bounds a single background re-index cycle.
const jobTimeout = 30 * time.Minute

// Reindexer is the indexing surface required by the API.
type Reindexer interface {
	IndexRepo(ctx context.Context, repo string) (indexer.Summary, error)
	ListRepos() ([]string, error)
}

// Config configures the management server.
type Config struct {
	Addr          string
	APIToken      string
	WorkspaceRoot string
}

// Server owns the HTTP listener and the background worker bookkeeping.
type Server struct {
	cfg     Config
	indexer Reindexer
	log     *slog.Logger
	jobs    *JobTracker
	http    *http.Server
	// baseCtx outlives individual requests so background workers survive the
	// HTTP response being flushed.
	baseCtx context.Context
}

// New builds the management server bound to cfg.Addr.
func New(baseCtx context.Context, cfg Config, idx Reindexer, log *slog.Logger) (*Server, error) {
	if idx == nil || log == nil {
		return nil, errors.New("httpapi: indexer and logger are required")
	}
	if strings.TrimSpace(cfg.Addr) == "" {
		return nil, errors.New("httpapi: listen address must not be empty")
	}

	s := &Server{
		cfg:     cfg,
		indexer: idx,
		log:     log,
		jobs:    NewJobTracker(),
		baseCtx: baseCtx,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/{repo_name}/update", s.handleUpdate)
	mux.HandleFunc("GET /api/v1/repos", s.handleListRepos)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.withRecovery(s.withLogging(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	return s, nil
}

// ListenAndServe blocks until the server is shut down.
func (s *Server) ListenAndServe() error {
	s.log.Info("management API listening", "addr", s.cfg.Addr, "auth_required", s.cfg.APIToken != "")
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("management API: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown management API: %w", err)
	}
	return nil
}

// handleUpdate triggers a repository-scoped Filter-Then-Parse cycle.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
		return
	}

	repo := r.PathValue("repo_name")
	if err := utils.ValidateRepoName(repo); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "repo": repo})
		return
	}
	// Prove the repository exists inside the workspace before claiming a slot.
	if _, err := utils.SafeRepoPath(s.cfg.WorkspaceRoot, repo); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error(), "repo": repo})
		return
	}

	started, since := s.jobs.TryStart(repo)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "an indexing cycle is already running for this repository",
			"repo":       repo,
			"running_ms": time.Since(since).Milliseconds(),
		})
		return
	}

	go s.runIndexJob(repo)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     "accepted",
		"repo":       repo,
		"started_at": since.UTC().Format(time.RFC3339Nano),
	})
}

// runIndexJob is the isolated background worker. The execution flag is cleared
// by a deferred teardown once the BadgerDB writes have been flushed.
func (s *Server) runIndexJob(repo string) {
	defer s.jobs.Finish(repo)
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("indexing worker panicked", "repo", repo, "panic", rec)
		}
	}()

	ctx, cancel := context.WithTimeout(s.baseCtx, jobTimeout)
	defer cancel()

	summary, err := s.indexer.IndexRepo(ctx, repo)
	if err != nil {
		s.log.Error("on-demand indexing failed", "repo", repo, "error", err)
		return
	}
	s.log.Info("on-demand indexing complete",
		"repo", repo,
		"files_parsed", summary.FilesParsed,
		"symbols", summary.SymbolsFound,
		"written", summary.RecordsWritten,
		"pruned", summary.RecordsPruned,
		"duration_ms", summary.DurationMS,
	)
}

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
		return
	}

	repos, err := s.indexer.ListRepos()
	if err != nil {
		s.log.Error("failed to list repositories", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list repositories"})
		return
	}

	type repoState struct {
		Name     string `json:"name"`
		Indexing bool   `json:"indexing"`
	}
	out := make([]repoState, 0, len(repos))
	for _, name := range repos {
		out = append(out, repoState{Name: name, Indexing: s.jobs.Active(name)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// authorized enforces the optional bearer token using a constant-time compare.
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.APIToken == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.APIToken)) == 1
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("http handler panicked", "path", r.URL.Path, "panic", rec)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// writeJSON emits a JSON body, never leaking internal details on encode errors.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written; there is nothing left to do but
		// let the connection close.
		_ = err
	}
}
