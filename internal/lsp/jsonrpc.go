package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
)

const (
	// JSON-RPC 2.0 version string.
	JsonRpcVer = "2.0"

	// MaxMessageBytes rejects absurd Content-Length headers before allocating. A
	// gopls workspace/symbol response on a large module stays far below this.
	MaxMessageBytes = 64 << 20 // 64 MiB
)

// RequestMessage is an outbound call; ID is nil for notifications.
type RequestMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ResponseMessage is an inbound reply to one of our requests.
type ResponseMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// serverReply answers a server-initiated request.
type serverReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

// ResponseError is a JSON-RPC error object returned by the language server.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *ResponseError) Error() string {
	return fmt.Sprintf("lsp: server error %d: %s", e.Code, e.Message)
}

// InboundMessage is the discriminating shape used to classify any received frame.
type InboundMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *ResponseError  `json:"error"`
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

// writeFrame emits one LSP frame: the Content-Length header, a blank line and
// the raw JSON payload.
func WriteFrame(w io.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return fmt.Errorf("lsp: write frame header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("lsp: write frame body: %w", err)
	}
	return nil
}

func CreateJsonRpcRequest(id *int64, method string, params any) ([]byte, error) {
	req := RequestMessage{
		JSONRPC: JsonRpcVer,
		ID:      id,
		Method:  method,
	}
	if params != nil {
		var err error
		req.Params, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("lsp: encode %s params: %w", method, err)
		}
	}
	return json.Marshal(req)
}

// frameReader decodes the Content-Length framing from a language server.
type frameReader struct {
	tp *textproto.Reader
}

// newFrameReader wraps r with the buffering the header parser requires.
func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{tp: textproto.NewReader(bufio.NewReaderSize(r, 64<<10))}
}

// read returns the next JSON payload, or an error. io.EOF is returned verbatim
// so the supervisor can distinguish a clean exit from a protocol violation.
func (f *frameReader) read() ([]byte, error) {
	header, err := f.tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	raw := header.Get("Content-Length")
	if raw == "" {
		return nil, errors.New("frame is missing the Content-Length header")
	}
	size, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed Content-Length %q: %w", raw, err)
	}
	if size < 0 || size > MaxMessageBytes {
		return nil, errors.New("frame data exceeding max size")
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(f.tp.R, payload); err != nil {
		return nil, fmt.Errorf("short read on a %d byte frame: %w", size, err)
	}
	return payload, nil
}
