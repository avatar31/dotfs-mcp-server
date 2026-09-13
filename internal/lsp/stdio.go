package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strconv"
	"sync"

	"go.uber.org/zap"
)

type stdio struct {
	clientName string
	writeMu    sync.Mutex
	writer     io.WriteCloser

	reader *textproto.Reader
	log    *zap.Logger
}

// newStdio wraps r with the buffering the header parser requires.
func newStdio(name string, w io.WriteCloser, r io.Reader, log *zap.Logger) *stdio {
	return &stdio{
		clientName: name,
		writer:     w,
		reader:     textproto.NewReader(bufio.NewReaderSize(r, 64<<10)),
		log:        log.With(zap.String("client", name)),
	}
}

// readLoop decodes frames until the stream ends or breaks.
func (s *stdio) readLoop(dispatch func(msg inboundMessage)) error {
	for {
		payload, err := s.read()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
				return fmt.Errorf("lsp: %s closed stdout pipe: %w", s.clientName, err)
			}
			return fmt.Errorf("lsp: %s stream error: %w", s.clientName, err)
		}

		var msg inboundMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			s.log.Warn("discarding unparsable lsp frame", zap.Error(err))
			continue
		}

		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			s.answerRequest(s.clientName, msg)
		case msg.Method != "":
			s.log.Debug("lsp notification", zap.Any("inbound-message", msg))
		default:
			dispatch(msg)
		}
	}
}

// answerServerRequest keeps the daemon unblocked. gopls in particular stalls
// indefinitely on an unanswered workspace/configuration or registerCapability.
func (s *stdio) answerRequest(clientName string, msg inboundMessage) {
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

	params := serverReply{
		baseJsonRpc: baseJsonRpc{JSONRPC: JsonRpcVer},
		ID:          msg.ID,
		Result:      result,
	}
	if err := s.write(params); err != nil {
		s.log.Warn("failed to answer server request",
			zap.String("method", msg.Method),
			zap.Error(err),
		)
	}
}

// read returns the next JSON payload, or an error. io.EOF is returned verbatim
// so the supervisor can distinguish a clean exit from a protocol violation.
func (s *stdio) read() ([]byte, error) {
	header, err := s.reader.ReadMIMEHeader()
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
	if _, err := io.ReadFull(s.reader.R, payload); err != nil {
		return nil, fmt.Errorf("short read on a %d byte frame: %w", size, err)
	}
	return payload, nil
}

// https://microsoft.github.io/language-server-protocol/specifications/lsp/3.18/specification/#contentPart
func (s *stdio) write(body any) error {
	s.log.Debug("LSP message", zap.Any("outbound-message", body))

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("lsp: marshal frame: %w", err)
	}

	if _, err := fmt.Fprintf(s.writer, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return fmt.Errorf("lsp: write frame header: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.writer.Write(payload); err != nil {
		return fmt.Errorf("lsp: write frame body: %w", err)
	}
	return nil
}

func (s *stdio) close() error {
	return s.writer.Close()
}
