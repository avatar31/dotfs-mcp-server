package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/avatar31/dotfs-mcp-server/internal/config"
	"github.com/avatar31/dotfs-mcp-server/internal/utils"
)

// Sentinel failures the tool layer maps onto actionable guidance for the utils.
var (
	// ErrNoCompileCommands means clangd would not be able to resolve headers.
	ErrNoCompileCommands = errors.New("lsp: no compile_commands.json was found for this repository")
	// ErrNoGoModule means gopls has no module to load.
	ErrNoGoModule = errors.New("lsp: no go.mod was found for this repository")
	// ErrClosed is returned once the manager has been shut down.
	ErrClosed = errors.New("lsp: manager is closed")
	// ErrDaemonExited is returned to every in-flight call when a daemon dies.
	ErrDaemonExited = errors.New("lsp: language server exited")
)

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
	cfg *config.Config
	log *zap.Logger

	mu      sync.Mutex
	clients map[string]*Client
	pending map[string]*spawnAttempt
	closed  bool
}

// NewManager builds a pool. It performs no I/O and never fails, so a workspace
// without any language server installed still boots normally.
func NewManager(cfg *config.Config, log *zap.Logger) *Manager {
	return &Manager{
		cfg:     cfg,
		log:     log,
		clients: make(map[string]*Client),
		pending: make(map[string]*spawnAttempt),
	}
}

// RequestTimeout exposes the configured per-call ceiling.
func (m *Manager) RequestTimeout() time.Duration { return m.cfg.LSPConfig.RequestTimeout }

// ClientSession returns a ready session for repoDir, spawning and initialising one if necessary
func (m *Manager) ClientSession(ctx context.Context, repo, repoDir string, lang utils.Language) (*Client, error) {
	key := repo + "|" + string(lang)

	// Bounded retry: an attempt only repeats when the session we were handed had
	// already died, which is rare and always makes progress towards a fresh spawn.
	for try := 0; try < 3; try++ {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		if existing, ok := m.clients[key]; ok {
			if existing.Alive() {
				m.mu.Unlock()
				return existing, nil
			}
			// The daemon crashed. Drop the reference and respawn below; the dead
			// client's supervisor goroutine has already reaped the process.
			delete(m.clients, key)
			m.log.Warn("language server died, respawning", zap.String("repo", repo),
				zap.String("language", string(lang)), zap.Error(existing.Err()))
		}
		if inflight, ok := m.pending[key]; ok {
			m.mu.Unlock()
			select {
			case <-inflight.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if inflight.err != nil {
				return nil, inflight.err
			}
			if inflight.client.Alive() {
				return inflight.client, nil
			}
			continue
		}

		// No existing session and no inflight spawn: Start new one.
		attempt := &spawnAttempt{done: make(chan struct{})}
		m.pending[key] = attempt
		m.mu.Unlock()

		client, err := m.spawn(ctx, repo, repoDir, lang)

		var orphan *Client
		m.mu.Lock()
		delete(m.pending, key)
		switch {
		case err != nil:
		case m.closed:
			// The pool was closed while this daemon was starting up; it belongs to
			// nobody, so it must be torn down instead of leaking.
			orphan, client, err = client, nil, ErrClosed
		default:
			m.clients[key] = client
		}
		attempt.client, attempt.err = client, err
		m.mu.Unlock()
		close(attempt.done)

		if orphan != nil {
			_ = orphan.Shutdown(context.Background())
		}
		if err != nil {
			return nil, err
		}
		return client, nil
	}

	return nil, fmt.Errorf("%w: failed to initiate client for %s", ErrDaemonExited, key)
}

// spawn starts a daemon and completes the initialize/initialized handshake with LSP.
func (m *Manager) spawn(ctx context.Context, repo, repoDir string, lang utils.Language) (*Client, error) {
	bin, args, err := m.commandFor(repoDir, lang)
	if err != nil {
		return nil, err
	}

	resolved, _ := exec.LookPath(bin)
	// The command is intentionally detached from the parent context: a daemon
	// must outlive the single tool call that happened to start it, and teardown
	// is handled explicitly by Shutdown.
	cmd := exec.Command(resolved, args...)
	cmd.Dir = repoDir
	cmd.Stderr = os.Stderr // stdout is the LSP channel; logs must not pollute it
	isolateProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe for %s: %w", bin, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe for %s: %w", bin, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", bin, err)
	}

	name := fmt.Sprintf("%s[%s]", filepath.Base(bin), repo)
	client := newClient(name, stdin, stdout, cmd, m.log)
	m.log.Info("language server started", zap.String("server", name),
		zap.Int("pid", cmd.Process.Pid), zap.String("dir", repoDir))

	initCtx, cancel := context.WithTimeout(ctx, m.cfg.LSPConfig.InitTimeout)
	defer cancel()
	if err := client.Initialize(initCtx, m.cfg, repoDir); err != nil {
		_ = client.Shutdown(context.Background())
		return nil, err
	}
	return client, nil
}

// commandFor validates the per-language prerequisites and builds the argv.
func (m *Manager) commandFor(repoDir string, lang utils.Language) (string, []string, error) {
	switch lang {
	case utils.LanguageGo:
		if !hasGoModule(repoDir) {
			return "", nil, fmt.Errorf("%w: %s", ErrNoGoModule, repoDir)
		}
		// gopls with no subcommand speaks LSP over stdio.
		return m.cfg.LSPConfig.GoplsPath, nil, nil

	case utils.LanguageC:
		dir, ok := compileCommandsDir(repoDir)
		if !ok {
			return "", nil, fmt.Errorf("%w: %s", ErrNoCompileCommands, repoDir)
		}
		args := []string{
			"--compile-commands-dir=" + dir,
			"--pch-storage=memory",
			"--log=error",
		}
		return m.cfg.LSPConfig.ClangdPath, append(args, m.cfg.LSPConfig.ClangdArgs...), nil

	default:
		return "", nil, fmt.Errorf("%w: %q", utils.ErrUnsupportedLanguage, lang)
	}
}

// Close shuts every daemon down. It is safe to call more than once.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = make(map[string]*Client)
	m.mu.Unlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(clients))
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			if err := c.Shutdown(ctx); err != nil {
				errCh <- err
			}
		}(c)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// IsMethodNotFound reports whether err is an unsupported-capability rejection,
// which callers translate into a graceful degradation rather than a failure.
func IsMethodNotFound(err error) bool {
	var re *ResponseError
	if !errors.As(err, &re) {
		return false
	}
	return re.Code == ErrCodeMethodNotFound
}

// hasGoModule reports whether gopls has a module to load. A go.mod one level
// down is accepted because service repositories frequently nest the module.
func hasGoModule(repoDir string) bool {
	if fileExists(filepath.Join(repoDir, "go.mod")) {
		return true
	}
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(repoDir, e.Name(), "go.mod")) {
			return true
		}
	}
	return false
}

// compileCommandsDir locates the compilation database clangd needs to resolve
// include paths, checking the conventional out-of-tree build directories.
// https://clangd.llvm.org/installation#project-setup
func compileCommandsDir(repoDir string) (string, bool) {
	for _, candidate := range []string{"", "build", "out", "_build", "cmake-build-debug"} {
		dir := filepath.Join(repoDir, candidate)
		if fileExists(filepath.Join(dir, "compile_commands.json")) {
			return dir, true
		}
	}
	return "", false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isolateProcessGroup puts the daemon in its own process group so signals reach
// the whole tree - clangd forks helper processes that would otherwise survive.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateTree sends SIGTERM to the daemon's process group.
func terminateTree(cmd *exec.Cmd) error {
	return signalTree(cmd, syscall.SIGTERM)
}

// killTree sends SIGKILL to the daemon's process group.
func killTree(cmd *exec.Cmd) error {
	return signalTree(cmd, syscall.SIGKILL)
}

func signalTree(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// The group is already gone; fall back to the single process.
		return cmd.Process.Signal(sig)
	}
	return syscall.Kill(-pgid, sig)
}
