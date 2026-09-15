package memory

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxRAGResponseBytes = 4 << 20
	maxRAGErrorBytes    = 4 << 10
	maxMCPSSELineBytes  = 64 << 10 // Includes the line terminator.
)

var errRAGResponseLimit = errors.New("RAG response exceeds byte limit")

// readRAGResponse enforces the budget on the bytes actually delivered by the
// HTTP body (after transport decompression), not untrusted Content-Length.
func readRAGResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxRAGResponseBytes+1))
	if len(data) > maxRAGResponseBytes {
		return nil, fmt.Errorf("%w (%d bytes)", errRAGResponseLimit, maxRAGResponseBytes)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Retain only the protocol classification, never a raw upstream error body.
type ragHTTPStatusError struct {
	statusCode  int
	requiresMCP bool
}

func (e *ragHTTPStatusError) Error() string {
	return fmt.Sprintf("third-party RAG returned status %d", e.statusCode)
}

func readRAGHTTPError(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRAGErrorBytes))
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ragHTTPStatusError{statusCode: resp.StatusCode, requiresMCP: hasMCPInitializationHint(string(data))}
}

func requiresMCPInitialization(err error) bool {
	var statusErr *ragHTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.requiresMCP
	}
	return hasMCPInitializationHint(err.Error())
}

func hasMCPInitializationHint(message string) bool {
	return strings.Contains(message, "invalid message version tag") ||
		strings.Contains(message, "expected \"2.0\"") ||
		strings.Contains(message, "malformed payload") ||
		strings.Contains(message, "invalid during session initialization") ||
		strings.Contains(message, "session") ||
		strings.Contains(message, "jsonrpc")
}

// readMCPSSEEndpoint bounds both individual lines and the entire discovery
// stream. A timeout closes the body to interrupt a blocked network Read. On
// success, stop that close callback so the SSE session remains alive during
// the subsequent POST flow; queryMCP owns its final Close.
func readMCPSSEEndpoint(ctx context.Context, body io.ReadCloser) (endpoint string, err error) {
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	closed := make(chan struct{})
	stopClose := context.AfterFunc(readCtx, func() {
		defer close(closed)
		_ = body.Close()
	})
	defer func() {
		if !stopClose() {
			<-closed
		}
		// Account for cancellation racing with the final endpoint read. Budget
		// violations must remain terminal, even if the timer also expires.
		if ctxErr := readCtx.Err(); ctxErr != nil && !errors.Is(err, errRAGResponseLimit) {
			endpoint, err = "", ctxErr
		}
		cancel()
	}()

	reader := bufio.NewReaderSize(io.LimitReader(body, maxRAGResponseBytes+1), maxMCPSSELineBytes+1)
	var total int
	var lastEvent string
	for {
		if err := readCtx.Err(); err != nil {
			return "", err
		}
		line, readErr := reader.ReadSlice('\n')
		total += len(line)
		if total > maxRAGResponseBytes {
			return "", fmt.Errorf("MCP SSE handshake: %w (%d bytes)", errRAGResponseLimit, maxRAGResponseBytes)
		}
		if len(line) > maxMCPSSELineBytes {
			return "", fmt.Errorf("MCP SSE line: %w (%d bytes)", errRAGResponseLimit, maxMCPSSELineBytes)
		}
		if readErr != nil {
			return "", readErr
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("event:")) {
			lastEvent = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
		} else if lastEvent == "endpoint" && bytes.HasPrefix(line, []byte("data:")) {
			return string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))), nil
		}
	}
}
