//go:build unix

package containersandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only; never contacts Docker or pulls images in normal tests.
func liveConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("AI_AGENT_RUN_CONTAINER_TESTS") != "true" {
		t.Skip("real Docker acceptance not enabled")
	}
	c := Config{Mode: "docker", SocketPath: os.Getenv("AI_AGENT_CONTAINER_TEST_SOCKET"), Image: os.Getenv("AI_AGENT_CONTAINER_TEST_IMAGE"), User: "65532:65532", CPUs: 1, MemoryMiB: 256, PidsLimit: 32, TmpfsMiB: 64}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}
func liveWorkspace(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0777); err != nil {
		t.Fatal(err)
	}
	return p
}
func liveRun(t *testing.T, c Config, work, program string, args []string, duration time.Duration) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), duration)
	defer cancel()
	var out, stderr bytes.Buffer
	err := Run(ctx, c, work, program, args, nil, &out, &stderr)
	return out.String(), stderr.String(), err
}
func TestDockerLive(t *testing.T) {
	c := liveConfig(t)
	t.Run("boundaries", func(t *testing.T) {
		work := liveWorkspace(t)
		outside := filepath.Join(liveWorkspace(t), "host-sentinel")
		if err := os.WriteFile(outside, []byte("host-only"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(work, "escape")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OPENAI_API_KEY", "service-sentinel")
		code := `import os,socket
assert os.getuid()==65532
s=open('/proc/self/status').read()
assert 'NoNewPrivs:\t1' in s and 'CapEff:\t0000000000000000' in s and 'Seccomp:\t2' in s
assert not os.getenv('OPENAI_API_KEY')
assert not os.path.exists('/var/run/docker.sock')
assert not os.path.exists('/workspace/escape')
try: open('/etc/host-write','w'); raise AssertionError('writable root')
except OSError: pass
try: socket.create_connection(('1.1.1.1',443),1); raise AssertionError('external network')
except OSError: pass
assert open('/sys/fs/cgroup/memory.max').read().strip()=='268435456'
assert open('/sys/fs/cgroup/pids.max').read().strip()=='32'
assert open('/sys/fs/cgroup/cpu.max').read().strip()=='100000 100000'
open('/workspace/result','w').write('isolated')
print('boundaries verified')`
		out, stderr, err := liveRun(t, c, work, "python3", []string{"-c", code}, 20*time.Second)
		if err != nil {
			t.Fatalf("%v; stdout=%s stderr=%s", err, out, stderr)
		}
		if b, err := os.ReadFile(filepath.Join(work, "result")); err != nil || string(b) != "isolated" {
			t.Fatal("workspace write failed")
		}
	})
	t.Run("toolchains", func(t *testing.T) {
		cc := c
		cc.MemoryMiB = 1024
		cc.TmpfsMiB = 512
		cc.PidsLimit = 128
		cc.CPUs = 2
		work := liveWorkspace(t)
		for path, body := range map[string]string{"go.mod": "module example.test/isolated\n\ngo 1.26.8\n", "main_test.go": "package isolated\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n"} {
			if err := os.WriteFile(filepath.Join(work, path), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
		for _, cmd := range []struct {
			name string
			args []string
		}{{"go", []string{"test", "./..."}}, {"node", []string{"-e", "console.log('node-ok')"}}, {"python3", []string{"-c", "print('python-ok')"}}} {
			out, stderr, err := liveRun(t, cc, work, cmd.name, cmd.args, 120*time.Second)
			if err != nil {
				t.Fatalf("%s: %v %s %s", cmd.name, err, out, stderr)
			}
		}
		_, _, err := liveRun(t, c, work, "rg", []string{"no-such-text", "go.mod"}, 15*time.Second)
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code != 1 {
			t.Fatalf("rg exit contract: %v", err)
		}
	})
	t.Run("pids", func(t *testing.T) {
		code := `import subprocess
children=[]
try:
 for i in range(100): children.append(subprocess.Popen(['sleep','10']))
 raise AssertionError('PID limit not enforced')
except BlockingIOError: print('PID limit enforced')
finally:
 for p in children: p.kill()
 for p in children: p.wait()`
		out, stderr, err := liveRun(t, c, liveWorkspace(t), "python3", []string{"-c", code}, 20*time.Second)
		if err != nil || !strings.Contains(out, "PID limit enforced") {
			t.Fatalf("%v %s %s", err, out, stderr)
		}
	})
	t.Run("memory", func(t *testing.T) {
		cc := c
		cc.MemoryMiB = 64
		cc.TmpfsMiB = 16
		_, stderr, err := liveRun(t, cc, liveWorkspace(t), "python3", []string{"-c", "a=[]\nwhile True: a.append(bytearray(8*1024*1024))"}, 20*time.Second)
		var exit *ExitError
		if !errors.As(err, &exit) || !exit.OOMKilled {
			t.Fatalf("memory limit: %v %s", err, stderr)
		}
	})
	t.Run("cancel-descendants", func(t *testing.T) {
		work := liveWorkspace(t)
		ctx, cancel := context.WithCancel(t.Context())
		ctx, deadline := context.WithTimeout(ctx, 20*time.Second)
		defer deadline()
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, c, work, "python3", []string{"-c", `import subprocess,time
subprocess.Popen(['python3','-c',"import time;time.sleep(2);open('/workspace/orphan','w').write('leaked')"],start_new_session=True)
open('/workspace/ready','w').write('ready')
time.sleep(30)`}, nil, io.Discard, io.Discard)
		}()
		ready := false
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(filepath.Join(work, "ready")); err == nil {
				ready = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if !ready {
			t.Fatal("command did not launch its detached child")
		}
		time.Sleep(2500 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(work, "orphan")); !os.IsNotExist(err) {
			t.Fatal("detached child survived cancellation")
		}
	})
	t.Run("watchdog", func(t *testing.T) {
		// Deliberately run without the controller's cancellation; the in-container
		// watchdog must stop it independently. Cleanup still removes only this ID.
		cl := newClient(c.SocketPath)
		defer cl.transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		var image imageInfo
		_, err := cl.control(ctx, http.MethodGet, "/images/"+c.Image+"/json", nil, &image, 200)
		if err != nil {
			t.Fatal(err)
		}
		nonce := "live-watchdog-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
		name := "ai-agent-task-" + nonce
		spec, err := buildSpec(c, image, liveWorkspace(t), "sleep", []string{"30"}, nil, nil, nonce, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var created struct {
			ID string `json:"Id"`
		}
		_, err = cl.control(ctx, http.MethodPost, "/containers/create?name="+name, spec, &created, 201)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cl.removeOwned(created.ID, name, nonce); err != nil {
				t.Error(err)
			}
		}()
		_, err = cl.control(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, 204)
		if err != nil {
			t.Fatal(err)
		}
		var waited struct{ StatusCode int }
		_, err = cl.call(ctx, http.MethodPost, "/containers/"+created.ID+"/wait?condition=not-running", nil, &waited, 200)
		if err != nil || waited.StatusCode != 137 {
			t.Fatalf("watchdog failed: %v code=%d", err, waited.StatusCode)
		}
	})
}
