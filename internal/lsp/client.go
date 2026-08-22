package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// terminationGrace is the window a daemon gets between SIGTERM and SIGKILL.
const terminationGrace = 2 * time.Second

// Client is a supervised JSON-RPC session with one language server.
//
// A Client is safe for concurrent use: requests are multiplexed over a single
// pipe pair and correlated by numeric id. Exactly one goroutine owns the read
// side, so the child process can never be reaped while a frame is in flight.
type Client struct {
	name  string
	cmd   *exec.Cmd
	stdio *stdio
	log   *slog.Logger

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *ResponseMessage
	opened  map[string]struct{}

	shutdownOnce sync.Once

	done   chan struct{} // closed when the read loop stops
	exited chan struct{} // closed after the child has been reaped

	errMu   sync.Mutex
	exitErr error
}

// newClient wires a client onto an arbitrary stream pair. cmd may be nil, which
// is what the unit tests use to drive an in-process fake server.
func newClient(name string, stdin io.WriteCloser, stdout io.Reader, cmd *exec.Cmd, log *slog.Logger) *Client {
	stdio := newStdio(stdin, stdout, log)
	c := &Client{
		name:    name,
		log:     log,
		cmd:     cmd,
		stdio:   stdio,
		pending: make(map[int64]chan *ResponseMessage),
		opened:  make(map[string]struct{}),
		done:    make(chan struct{}),
		exited:  make(chan struct{}),
	}

	go c.supervise()
	return c
}

// Initialize performs the LSP handshake with a cold daemon. It is called once per
// daemon and must complete before the first request is sent.
func (c *Client) Initialize(ctx context.Context, cfg *Config, repoDir string) error {
	params := InitializeParams{
		ProcessID: int64(os.Getpid()),
		ClientInfo: ClientInfo{
			Name:    cfg.ClientName,
			Version: cfg.ClientVersion,
		},
		RootPath: repoDir,
		RootURI:  PathToURI(repoDir),
		Capabilities: ClientCapabilities{
			Workspace: &WorkspaceClientCapabilities{WorkspaceFolders: true, Configuration: true},
			Window:    &WindowClientCapabilities{WorkDoneProgress: true},
			TextDocument: &TextDocumentClientCapabilities{
				Synchronization: map[string]any{"dynamicRegistration": false},
				References:      map[string]any{"dynamicRegistration": false},
				Definition:      map[string]any{"dynamicRegistration": false},
				TypeDefinition:  map[string]any{"dynamicRegistration": false},
				Implementation:  map[string]any{"dynamicRegistration": false},
				CallHierarchy:   map[string]any{"dynamicRegistration": false},
				TypeHierarchy:   map[string]any{"dynamicRegistration": false},
			},
		},
		WorkspaceFolders: []*WorkspaceFolder{{
			URI:  PathToURI(repoDir),
			Name: filepath.Base(repoDir),
		}},
	}

	if err := c.Call(ctx, MethodInitialize, params, nil); err != nil {
		return fmt.Errorf("lsp: initialize %s: %w", c.Name(), err)
	}
	if err := c.Notify(MethodInitialized, map[string]any{}); err != nil {
		return fmt.Errorf("lsp: initialized %s: %w", c.Name(), err)
	}
	return nil
}

// Name is the daemon identity used in logs and error messages.
func (c *Client) Name() string { return c.name }

// Alive reports whether the session can still serve requests.
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// supervise runs the read loop and then reaps the child. Reaping strictly after
// the loop returns satisfies the os/exec contract that Wait must not race with
// readers of the stdout pipe.
func (c *Client) supervise() {
	err := c.stdio.readLoop(c.name, c.dispatch)
	c.fail(err)

	if c.cmd != nil {
		if waitErr := c.cmd.Wait(); waitErr != nil {
			c.log.Warn("language server exited with error", "server", c.name, "error", waitErr)
		}
	}
	close(c.exited)
}

// dispatch hands a reply to the goroutine blocked in Call.
func (c *Client) dispatch(msg inboundMessage) {
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		c.log.Warn("response carries a non-numeric id", "server", c.name, "id", string(msg.ID))
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if !ok {
		// A cancelled call already gave up; dropping the reply is correct.
		c.log.Warn("no pending call for response", "server", c.name, "id", id)
		return
	}
	ch <- &ResponseMessage{ID: msg.ID, Result: msg.Result, Error: msg.Error}
}

// fail tears down every in-flight call once the transport is gone.
func (c *Client) fail(err error) {
	c.errMu.Lock()
	if c.exitErr == nil {
		c.exitErr = err
	}
	c.errMu.Unlock()

	// Close all pending calls so they don't block forever. The read loop has exited
	// so no new calls will be added to the map.
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan *ResponseMessage)
	c.mu.Unlock()

	close(c.done)
	for id, ch := range pending {
		ch <- &ResponseMessage{Error: &ResponseError{Code: ErrCodeRequestFailed, Message: err.Error()}}
		c.log.Debug("aborted in-flight lsp request", "server", c.name, "id", id)
	}
}

// Err reports why the session ended, or nil while it is healthy.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.exitErr
}

// Call sends an LSP request and decodes the response into out, which may be nil when
// the reply is irrelevant. The context bounds the wait and cancels server-side.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	if !c.Alive() {
		return fmt.Errorf("call %s: %w", method, c.exitReason())
	}

	id, ch := c.trackCall()
	req, err := newRequestMessage(&id, method, params)
	if err != nil {
		c.forgetCall(id)
		return fmt.Errorf("lsp: encode %s request: %w", method, err)
	}

	if err := c.stdio.write(req); err != nil {
		c.forgetCall(id)
		return err
	}

	select {
	case <-ctx.Done():
		c.forgetCall(id)
		// Best effort: tell the daemon to stop working on a result nobody wants.
		c.notifyBestEffort(methodCancelRequest, map[string]any{"id": id})
		return fmt.Errorf("lsp: %s on %s: %w", method, c.name, ctx.Err())

	case <-c.done:
		c.forgetCall(id)
		return fmt.Errorf("lsp: %s on %s: %w", method, c.name, c.exitReason())

	case reply := <-ch:
		if reply.Error != nil {
			return fmt.Errorf("lsp: %s on %s: %w", method, c.name, reply.Error)
		}
		if out == nil || len(reply.Result) == 0 || string(reply.Result) == "null" {
			return nil
		}
		if err := json.Unmarshal(reply.Result, out); err != nil {
			return fmt.Errorf("lsp: decode %s result: %w", method, err)
		}
		return nil
	}
}

// Notify sends an LSP notification without waiting for a response (fire-and-forget message).
func (c *Client) Notify(method string, params any) error {
	if !c.Alive() {
		return fmt.Errorf("notify %s: %w", method, c.exitReason())
	}

	req, err := newRequestMessage(nil, method, params)
	if err != nil {
		return fmt.Errorf("lsp: encode %s request: %w", method, err)
	}
	return c.stdio.write(req)
}

// notifyBestEffort swallows failures on teardown paths.
func (c *Client) notifyBestEffort(method string, params any) {
	if err := c.Notify(method, params); err != nil {
		c.log.Debug("best-effort notification failed", "server", c.name, "method", method, "error", err)
	}
}

// trackCall reserves a slot for a pending request and returns the id and channel to receive the reply.
func (c *Client) trackCall() (int64, chan *ResponseMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID
	ch := make(chan *ResponseMessage, 1)
	c.pending[id] = ch
	return id, ch
}

// forgetCall removes a pending slot after cancellation or a write failure.
func (c *Client) forgetCall(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// exitReason explains a dead session.
func (c *Client) exitReason() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrDaemonExited
}

// EnsureOpen publishes a document to the server exactly once per session.
// clangd will not answer positional requests for a document it has not seen.
func (c *Client) EnsureOpen(ctx context.Context, path, languageID string) error {
	c.mu.Lock()
	_, already := c.opened[path]
	c.mu.Unlock()
	if already {
		return nil
	}

	text, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lsp: read %s: %w", path, err)
	}

	// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#textDocument_didOpen
	err = c.Notify(MethodDidOpen, DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        PathToURI(path),
			LanguageID: languageID,
			Version:    1,
			Text:       string(text),
		},
	})
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.opened[path] = struct{}{}
	c.mu.Unlock()

	// Give the server a beat to index the freshly opened document; a positional
	// request issued in the same instant otherwise races the parse on clangd.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// Shutdown performs the graceful LSP handshake and then guarantees the child
// process tree is gone: SIGTERM, a two second grace window, then SIGKILL.
func (c *Client) Shutdown(ctx context.Context) error {
	var err error
	c.shutdownOnce.Do(func() { err = c.shutdown(ctx) })
	return err
}

func (c *Client) shutdown(ctx context.Context) error {
	if c.Alive() {
		// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#shutdown
		graceful, cancel := context.WithTimeout(ctx, terminationGrace)
		if callErr := c.Call(graceful, MethodShutdown, nil, nil); callErr != nil {
			c.log.Debug("graceful lsp shutdown failed", "server", c.name, "error", callErr)
		}
		cancel()
		c.notifyBestEffort(MethodExit, nil)
	}
	if err := c.stdio.close(); err != nil {
		c.log.Debug("closing lsp stdin failed", "server", c.name, "error", err)
	}

	if c.cmd == nil || c.cmd.Process == nil {
		<-c.exited
		return nil
	}

	if err := terminateTree(c.cmd); err != nil {
		c.log.Debug("SIGTERM delivery failed", "server", c.name, "error", err)
	}
	select {
	case <-c.exited:
		return nil
	case <-time.After(terminationGrace):
	}

	c.log.Warn("language server ignored SIGTERM, escalating to SIGKILL", "server", c.name)
	if err := killTree(c.cmd); err != nil {
		return fmt.Errorf("lsp: kill %s: %w", c.name, err)
	}
	<-c.exited
	return nil
}
