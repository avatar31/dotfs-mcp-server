package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/avatar31/dotfs-mcp-server/internal/model"
)

// Default lifecycle timings. RequestTimeout implements the 5 s ceiling from the
// error-handling matrix; InitTimeout is deliberately larger because a cold
// gopls has to load an entire module graph before it answers anything.
const (
	DefaultRequestTimeout = 5 * time.Second
	DefaultInitTimeout    = 45 * time.Second
	DefaultGoplsPath      = "gopls"
	DefaultClangdPath     = "clangd"
)

// Sentinel failures the tool layer maps onto actionable guidance for the model.
var (
	// ErrDisabled is returned when cross-reference support is switched off.
	ErrDisabled = errors.New("lsp: cross-reference engine is disabled")
	// ErrDaemonExited is returned to every in-flight call when a daemon dies.
	ErrDaemonExited = errors.New("lsp: language server exited")
)

// Config parameterises the daemon pool.
type Config struct {
	// Enabled switches the whole Phase 3 engine off.
	Enabled bool
	// GoplsPath / ClangdPath are executable names or absolute paths.
	GoplsPath  string
	ClangdPath string
	// ClangdArgs are appended to the generated clangd command line.
	ClangdArgs []string
	// RequestTimeout bounds a single JSON-RPC round trip.
	RequestTimeout time.Duration
	// InitTimeout bounds the initialize handshake of a cold daemon.
	InitTimeout time.Duration
	// ClientName / ClientVersion are announced during initialize.
	ClientName    string
	ClientVersion string
}

// withDefaults fills the zero values so callers only set what they care about.
func (c Config) withDefaults() Config {
	if c.GoplsPath == "" {
		c.GoplsPath = DefaultGoplsPath
	}
	if c.ClangdPath == "" {
		c.ClangdPath = DefaultClangdPath
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.InitTimeout <= 0 {
		c.InitTimeout = DefaultInitTimeout
	}
	if c.ClientName == "" {
		c.ClientName = "dotfs-mcp-server"
	}
	if c.ClientVersion == "" {
		c.ClientVersion = "1.0.0"
	}
	return c
}

// spawnAttempt lets concurrent callers share a single cold start. Without it a
// burst of relational tool calls would each launch their own gopls and then
// throw all but one away, multiplying the most expensive operation we have.
type spawnAttempt struct {
	done   chan struct{}
	client *Client
	err    error
}

// Manager owns one lazily spawned language server per (repository, language).
//
// Daemons are never started at boot: the first cross-reference tool call for a
// repository pays the initialisation cost, every later call reuses the session.
// A daemon that died is transparently replaced on the next request.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	clients map[string]*Client
	pending map[string]*spawnAttempt
	closed  bool
}

// NewManager builds a pool. It performs no I/O and never fails, so a workspace
// without any language server installed still boots normally.
func NewManager(cfg Config, log *slog.Logger) *Manager {
	return &Manager{
		cfg:     cfg.withDefaults(),
		log:     log,
		clients: make(map[string]*Client),
		pending: make(map[string]*spawnAttempt),
	}
}

// RequestTimeout exposes the configured per-call ceiling.
func (m *Manager) RequestTimeout() time.Duration { return m.cfg.RequestTimeout }

// ClientSession returns a ready session for repoDir, spawning and initialising one if necessary
func (m *Manager) ClientSession(ctx context.Context, repo, repoDir string, lang model.Language) (*Client, error) {
	if !m.cfg.Enabled {
		return nil, ErrDisabled
	}

	key := repo + "|" + string(lang)

	// Bounded retry: an attempt only repeats when the session we were handed had
	// already died, which is rare and always makes progress towards a fresh spawn.
	for try := 0; try < 3; try++ {
		// TODO: Implement
	}

	return nil, fmt.Errorf("%w: failed to initiate client for %s", ErrDaemonExited, key)
}

// Close shuts every daemon down. It is safe to call more than once.
func (m *Manager) Close(ctx context.Context) error {
	return nil
}
