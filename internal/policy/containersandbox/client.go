package containersandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

const maxMetadataBytes = 1 << 20

type client struct {
	socket    string
	http      *http.Client
	transport *http.Transport
}

func newClient(socket string) *client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: controlTimeout}).DialContext(ctx, "unix", socket)
	}, DisableCompression: true}
	return &client{socket: socket, transport: transport, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) call(ctx context.Context, method, path string, input, output any, want ...int) (int, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, errors.New("sandbox request could not be encoded")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+apiVersion+path, body)
	if err != nil {
		return 0, errors.New("sandbox request could not be constructed")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("sandbox local Docker API is unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil || len(data) > maxMetadataBytes {
		return response.StatusCode, errors.New("sandbox API metadata exceeded its bound or could not be read")
	}
	accepted := false
	for _, status := range want {
		if response.StatusCode == status {
			accepted = true
		}
	}
	if !accepted {
		return response.StatusCode, fmt.Errorf("sandbox API request failed with HTTP %d (response redacted)", response.StatusCode)
	}
	if output != nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		if decoder.Decode(output) != nil {
			return response.StatusCode, errors.New("sandbox API metadata is invalid")
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			return response.StatusCode, errors.New("sandbox API metadata is invalid")
		}
	}
	return response.StatusCode, nil
}

func (c *client) control(ctx context.Context, method, path string, input, output any, want ...int) (int, error) {
	controlCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	return c.call(controlCtx, method, path, input, output, want...)
}

func (c *client) checkDaemon(ctx context.Context) error {
	var info struct {
		OSType                                                       string
		MemoryLimit, SwapLimit, CpuCfsPeriod, CpuCfsQuota, PidsLimit bool
		SecurityOptions                                              []string
		Runtimes                                                     map[string]any
	}
	if _, err := c.control(ctx, http.MethodGet, "/info", nil, &info, http.StatusOK); err != nil {
		return err
	}
	seccomp := false
	for _, option := range info.SecurityOptions {
		if option == "name=seccomp,profile=builtin" {
			seccomp = true
		}
	}
	if info.OSType != "linux" || !info.MemoryLimit || !info.SwapLimit || !info.CpuCfsPeriod || !info.CpuCfsQuota || !info.PidsLimit || !seccomp || info.Runtimes["runc"] == nil {
		return errors.New("sandbox requires a Linux daemon with enforced memory/swap/CPU/PID limits, runc and the builtin seccomp profile")
	}
	return nil
}

func (c *client) inspect(ctx context.Context, id string) (containerInfo, int, error) {
	var result containerInfo
	status, err := c.control(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &result, http.StatusOK)
	return result, status, err
}

func (c *client) removeOwned(id, name, nonce string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	target := id
	if target == "" {
		target = name
	}
	for attempt := 0; attempt < 3; attempt++ {
		inspected, status, err := c.inspect(ctx, target)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil || !validContainerID(inspected.ID) || inspected.Config.Labels[ownerLabel] != nonce || (id != "" && inspected.ID != id) {
			return fmt.Errorf("%w: %s", ErrCleanup, target)
		}
		target = inspected.ID
		_, _ = c.control(ctx, http.MethodDelete, "/containers/"+target+"?force=1&v=0", nil, nil, http.StatusNoContent, http.StatusNotFound)
		_, status, _ = c.inspect(ctx, target)
		if status == http.StatusNotFound {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrCleanup, target)
}

type outputStream struct {
	reader     io.Reader
	connection net.Conn
	stop       func() bool
}

func (s *outputStream) Close() { s.stop(); _ = s.connection.Close() }

// Attach before start: unlike rotating daemon logs, this preserves stdout and
// stderr order within each stream while the caller drains past its output bound.
func (c *client) attach(ctx context.Context, id string) (*outputStream, error) {
	connection, err := (&net.Dialer{Timeout: controlTimeout}).DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, errors.New("sandbox output connection failed")
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	failed := true
	defer func() {
		if failed {
			stop()
			_ = connection.Close()
		}
	}()
	handshakeDeadline := time.Now().Add(controlTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	_ = connection.SetDeadline(handshakeDeadline)
	req, err := http.NewRequest(http.MethodPost, "http://docker"+apiVersion+"/containers/"+id+"/attach?stream=1&stdin=0&stdout=1&stderr=1", nil)
	if err != nil {
		return nil, errors.New("sandbox output request failed")
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if req.Write(connection) != nil {
		return nil, errors.New("sandbox output request failed")
	}
	buffered := bufio.NewReader(connection)
	response, err := http.ReadResponse(buffered, req)
	if err != nil || (response.StatusCode != http.StatusSwitchingProtocols && response.StatusCode != http.StatusOK) {
		return nil, errors.New("sandbox output attach failed (response redacted)")
	}
	var reader io.Reader = buffered
	if response.StatusCode == http.StatusOK {
		reader = response.Body
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Time{})
	}
	failed = false
	return &outputStream{reader: reader, connection: connection, stop: stop}, nil
}

func copyOutput(reader io.Reader, stdout, stderr io.Writer) error {
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(reader, header)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("sandbox output stream ended inside a frame")
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 || (header[0] != 1 && header[0] != 2) {
			return errors.New("sandbox output frame is invalid")
		}
		length := int64(binary.BigEndian.Uint32(header[4:]))
		if length > 16<<20 {
			return errors.New("sandbox output frame exceeds its bound")
		}
		target := stdout
		if header[0] == 2 {
			target = stderr
		}
		if _, err := io.CopyN(target, reader, length); err != nil {
			return errors.New("sandbox output frame could not be drained")
		}
	}
}

func checkSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return errors.New("sandbox local Unix socket is unavailable")
	}
	return nil
}
