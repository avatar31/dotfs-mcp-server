package xref

import (
	"context"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// Session is the slice of an LSP client the service depends on. Keeping it an
// interface makes the whole compaction pipeline testable without a daemon.
type Session interface {}

// Provider hands out initialised sessions, one per repository and language.
type Provider interface {
	ClientSession(ctx context.Context, repo, repoDir string, lang model.Language) (Session, error)
	RequestTimeout() time.Duration
}

// managerProvider adapts the concrete daemon pool onto the narrow interface the
// service consumes, which keeps the compaction pipeline free of process
// management concerns and trivially testable with a stub.
type managerProvider struct {
	mgr *lsp.Manager
}

// FromManager wraps an LSP daemon pool as a session provider.
func FromManager(mgr *lsp.Manager) Provider { return managerProvider{mgr: mgr} }

// Session lazily starts the language server owning repoDir.
func (p managerProvider) ClientSession(ctx context.Context, repo, repoDir string, lang model.Language) (Session, error) {
	client, err := p.mgr.ClientSession(ctx, repo, repoDir, lang)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// RequestTimeout exposes the configured per-call ceiling.
func (p managerProvider) RequestTimeout() time.Duration { return p.mgr.RequestTimeout() }
