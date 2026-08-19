package xref

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/lsp"
)

// Service answers relational queries by driving a language server and then
// compacting whatever it says.
type Service struct {
	provider Provider
	root     string
	log      *slog.Logger
	limit    int
	timeout  time.Duration
}

// New builds the service. workspaceRoot must be the same directory the indexer
// walks, because every result is reported relative to it.
func New(provider Provider, workspaceRoot string, log *slog.Logger) (*Service, error) {
	if provider == nil {
		return nil, errors.New("xref: a session provider is required")
	}
	if log == nil {
		return nil, errors.New("xref: a logger is required")
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("xref: resolve workspace root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	timeout := provider.RequestTimeout()
	if timeout <= 0 {
		timeout = lsp.DefaultRequestTimeout
	}
	return &Service{provider: provider, root: root, log: log, limit: MaxResults, timeout: timeout}, nil
}
