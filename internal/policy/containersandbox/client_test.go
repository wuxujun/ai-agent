//go:build unix

package containersandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDaemon struct {
	mu                                   sync.Mutex
	cfg                                  Config
	spec                                 containerSpec
	exists                               bool
	name                                 string
	started, deleted                     int
	exitCode                             int
	hold, lostCreate, denyDelete, tamper bool
	noSeccomp, badImage                  bool
	connection                           net.Conn
	done                                 chan struct{}
	doneOnce                             sync.Once
}

func newFake(t *testing.T) *fakeDaemon {
	t.Helper()
	if _, err := os.Stat("/private/tmp"); err == nil {
		t.Setenv("TMPDIR", "/private/tmp")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{cfg: testConfig(t), done: make(chan struct{})}
	f.cfg.SocketPath = socket
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		f.mu.Lock()
		if f.connection != nil {
			_ = f.connection.Close()
		}
		f.doneOnce.Do(func() { close(f.done) })
		f.mu.Unlock()
		_ = server.Close()
	})
	return f
}

func (f *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, apiVersion)
	if path == "/info" {
		options := []string{"name=seccomp,profile=builtin"}
		if f.noSeccomp {
			options = nil
		}
		json.NewEncoder(w).Encode(map[string]any{"OSType": "linux", "MemoryLimit": true, "SwapLimit": true, "CpuCfsPeriod": true, "CpuCfsQuota": true, "PidsLimit": true, "SecurityOptions": options, "Runtimes": map[string]any{"runc": map[string]any{}}})
		return
	}
	if strings.HasPrefix(path, "/images/") {
		image := testImage(f.cfg)
		if f.badImage {
			image.Config.Labels = nil
		}
		json.NewEncoder(w).Encode(image)
		return
	}
	if path == "/containers/create" {
		var spec containerSpec
		if json.NewDecoder(r.Body).Decode(&spec) != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.spec = spec
		f.name = r.URL.Query().Get("name")
		f.exists = true
		lost := f.lostCreate
		f.mu.Unlock()
		if lost {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"Id": strings.Repeat("b", 64)})
		return
	}
	if strings.HasSuffix(path, "/json") {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.exists {
			w.WriteHeader(404)
			return
		}
		info := infoForSpec(f.spec)
		select {
		case <-f.done:
			info.State.Status = "exited"
			info.State.ExitCode = f.exitCode
		default:
			if f.started > 0 {
				info.State.Status = "running"
				info.State.Running = true
			}
		}
		if f.tamper {
			info.HostConfig.NetworkMode = "host"
		}
		json.NewEncoder(w).Encode(info)
		return
	}
	if strings.HasSuffix(path, "/attach") {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = buffer.Flush()
		f.mu.Lock()
		f.connection = connection
		f.mu.Unlock()
		return
	}
	if strings.HasSuffix(path, "/start") {
		f.mu.Lock()
		f.started++
		hold := f.hold
		connection := f.connection
		f.mu.Unlock()
		w.WriteHeader(204)
		if !hold {
			go func() {
				for _, part := range []struct {
					kind  byte
					value string
				}{{1, "stdout"}, {2, "stderr"}} {
					header := make([]byte, 8)
					header[0] = part.kind
					binary.BigEndian.PutUint32(header[4:], uint32(len(part.value)))
					_, _ = connection.Write(append(header, []byte(part.value)...))
				}
				_ = connection.Close()
				f.doneOnce.Do(func() { close(f.done) })
			}()
		}
		return
	}
	if strings.HasSuffix(path, "/wait") {
		select {
		case <-r.Context().Done():
			return
		case <-f.done:
			json.NewEncoder(w).Encode(map[string]any{"StatusCode": f.exitCode})
			return
		}
	}
	if r.Method == http.MethodDelete {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted++
		if f.denyDelete {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("private-upstream-secret"))
			return
		}
		f.exists = false
		if f.connection != nil {
			_ = f.connection.Close()
		}
		f.doneOnce.Do(func() { close(f.done) })
		w.WriteHeader(204)
		return
	}
	http.Error(w, "unexpected fake request", 400)
}

func TestDockerRun(t *testing.T) {
	for _, name := range []string{"success", "exit", "drift", "cancel", "lost-create", "cleanup-failure"} {
		t.Run(name, func(t *testing.T) {
			fake := newFake(t)
			switch name {
			case "exit":
				fake.exitCode = 7
			case "drift":
				fake.tamper = true
			case "cancel":
				fake.hold = true
			case "lost-create":
				fake.lostCreate = true
			case "cleanup-failure":
				fake.hold = true
				fake.denyDelete = true
			}
			timeout := 2 * time.Second
			if fake.hold || fake.lostCreate {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			result := Run(ctx, fake.cfg, workspace, "sh", []string{"-c", "printf ok"}, nil, &stdout, &stderr)
			if name == "success" && (result != nil || stdout.String() != "stdout" || stderr.String() != "stderr") {
				t.Fatalf("successful run failed: %v", result)
			}
			if name == "exit" {
				var exited *ExitError
				if !errors.As(result, &exited) || exited.ExitCode() != 7 {
					t.Fatalf("exit contract lost: %v", result)
				}
			}
			if (name == "cancel" || name == "lost-create") && !errors.Is(result, context.DeadlineExceeded) {
				t.Fatalf("cancellation contract lost: %v", result)
			}
			if name == "cleanup-failure" && !errors.Is(result, ErrCleanup) {
				t.Fatal("unconfirmed cleanup reported success")
			}
			if result != nil && strings.Contains(result.Error(), "private-upstream-secret") {
				t.Fatal("upstream response was exposed")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if name == "drift" && (result == nil || fake.started != 0) {
				t.Fatal("drifted container was started")
			}
			if name != "cleanup-failure" && (fake.exists || fake.deleted == 0) {
				t.Fatalf("owned container leaked in %s", name)
			}
		})
	}
}

func TestDockerHostSocket(t *testing.T) {
	fake := newFake(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = Run(ctx, fake.cfg, root, "sh", nil, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "host socket") {
		t.Fatalf("host IPC socket mount accepted: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.exists {
		t.Fatal("unsafe workspace reached container creation")
	}
}

func TestDockerOwnership(t *testing.T) {
	fake := newFake(t)
	spec, err := buildSpec(fake.cfg, testImage(fake.cfg), "/workspace", "sh", nil, nil, nil, "foreign", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fake.spec = spec
	fake.exists = true
	c := newClient(fake.cfg.SocketPath)
	defer c.transport.CloseIdleConnections()
	if err := c.removeOwned(strings.Repeat("b", 64), "unused", "own"); !errors.Is(err, ErrCleanup) {
		t.Fatal("foreign container was considered ours")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deleted != 0 {
		t.Fatal("foreign container removal attempted")
	}
}

func TestDockerErrorsRedacted(t *testing.T) {
	fake := newFake(t)
	c := newClient(fake.cfg.SocketPath)
	defer c.transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := c.control(ctx, http.MethodGet, "/not-found", nil, nil, 200)
	if err == nil || strings.Contains(fmt.Sprint(err), "unexpected fake request") {
		t.Fatal("upstream error response was exposed")
	}
}

func TestDockerPreflightRejectsUnsupportedRuntime(t *testing.T) {
	for _, mode := range []string{"seccomp", "image"} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t)
			f.noSeccomp = mode == "seccomp"
			f.badImage = mode == "image"
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if Preflight(ctx, f.cfg) == nil {
				t.Fatal("unsupported runtime or image accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.exists {
				t.Fatal("preflight created a container")
			}
		})
	}
}
