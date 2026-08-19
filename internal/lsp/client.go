package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

// Client is a supervised JSON-RPC session with one language server.
//
// A Client is safe for concurrent use: requests are multiplexed over a single
// pipe pair and correlated by numeric id. Exactly one goroutine owns the read
// side, so the child process can never be reaped while a frame is in flight.
type Client struct {
	name        string
	frameReader *frameReader
	cmd         *exec.Cmd
	log         *slog.Logger

	stdinWriteMu sync.Mutex
	stdin        io.WriteCloser

	mu      sync.Mutex
	pending map[int64]chan *ResponseMessage

	done   chan struct{} // closed when the read loop stops
	exited chan struct{} // closed after the child has been reaped

	errMu   sync.Mutex
	exitErr error
}

func NewClient(name string, stdin io.WriteCloser, stdout io.Reader, cmd *exec.Cmd, log *slog.Logger) *Client {
	c := &Client{
		name:        name,
		log:         log,
		stdin:       stdin,
		frameReader: newFrameReader(stdout),
		cmd:         cmd,
		pending:     make(map[int64]chan *ResponseMessage),
		done:        make(chan struct{}),
		exited:      make(chan struct{}),
	}

	go c.supervise()
	return c
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
	err := c.readLoop()
	c.fail(err)

	if c.cmd != nil {
		if waitErr := c.cmd.Wait(); waitErr != nil {
			c.log.Warn("language server exited with error", "server", c.name, "error", waitErr)
		}
	}
	close(c.exited)
}

func (c *Client) readLoop() error {
	for {
		payload, err := c.frameReader.read()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				return fmt.Errorf("lsp: %s closed stdout pipe: %w", c.name, err)
			}
			return fmt.Errorf("lsp: %s stream error: %w", c.name, err)
		}

		var msg InboundMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			c.log.Warn("discarding unparsable lsp frame", "server", c.name, "error", err)
			continue
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			c.answerRequest(msg)
		case msg.Method != "":
			c.log.Debug("lsp notification", "server", c.name, "method", msg.Method)
		default:
			c.dispatch(msg)
		}
	}
}

// dispatch hands a reply to the goroutine blocked in Call.
func (c *Client) dispatch(msg InboundMessage) {
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

// answerServerRequest keeps the daemon unblocked. gopls in particular stalls
// indefinitely on an unanswered workspace/configuration or registerCapability.
func (c *Client) answerRequest(msg InboundMessage) {
	var result any // JSON null unless a specific shape is mandated.
	if msg.Method == methodWorkspaceConfiguration {
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(msg.Params, &params); err == nil {
			settings := make([]map[string]any, len(params.Items))
			for i := range settings {
				settings[i] = map[string]any{}
			}
			result = settings
		} else {
			result = []map[string]any{}
		}
	}

	replay, err := json.Marshal(serverReply{
		JSONRPC: JsonRpcVer,
		ID:      msg.ID,
		Result:  result,
	})
	if err != nil {
		c.log.Error("failed to encode reply to server request", "server", c.name, "error", err)
		return
	}
	if err := c.writeRaw(replay); err != nil {
		c.log.Warn("failed to answer server request", "server", c.name, "method", msg.Method, "error", err)
	}
}

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
		ch <- &ResponseMessage{Error: &ResponseError{Code: CodeRequestFailed, Message: err.Error()}}
		c.log.Debug("aborted in-flight lsp request", "server", c.name, "id", id)
	}
}

func (c *Client) writeRaw(payload []byte) error {
	c.stdinWriteMu.Lock()
	defer c.stdinWriteMu.Unlock()
	return WriteFrame(c.stdin, payload)
}
