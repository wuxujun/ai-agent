//go:build darwin || linux

package restoreworkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func workspaceFixture(t *testing.T) (string, string) {
	t.Helper()
	sandbox, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sandbox, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(sandbox, RelativeRoot("task", "tenant"))
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("private-content"), 0600); err != nil {
		t.Fatal(err)
	}
	return sandbox, root
}
func TestScanWorkspaceReadOnlyAndRevision(t *testing.T) {
	sandbox, root := workspaceFixture(t)
	first, err := Scan(t.Context(), sandbox, "task", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Scan(t.Context(), sandbox, "task", "tenant")
	if err != nil || first.Revision != second.Revision || len(second.Files) != 1 || second.Files[0].Path != "sub/file.txt" || second.Files[0].SHA256 != digest([]byte("private-content")) {
		t.Fatal(first, second, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sub", "file.txt"))
	if err != nil || string(data) != "private-content" {
		t.Fatal("content changed", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("changed-content"), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := Scan(t.Context(), sandbox, "task", "tenant")
	if err != nil || third.Revision == first.Revision {
		t.Fatal("mutation not fingerprinted", err)
	}
	if _, err := Scan(t.Context(), sandbox, "task", "other-tenant"); !errors.Is(err, ErrMissing) {
		t.Fatal("tenant root shared", err)
	}
}
func TestScanWorkspaceUnsafeAndBudgets(t *testing.T) {
	cases := []struct {
		name     string
		expected error
		mutate   func(*testing.T, string)
	}{
		{"symlink_file", ErrUnsafe, func(t *testing.T, root string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink_directory", ErrUnsafe, func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "dir")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard_link", ErrUnsafe, func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, "sub", "file.txt"), filepath.Join(root, "alias")); err != nil {
				t.Fatal(err)
			}
		}},
		{"public_file", ErrUnsafe, func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "sub", "file.txt"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"public_directory", ErrUnsafe, func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, "sub"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", ErrUnsafe, func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized", ErrBudget, func(t *testing.T, root string) {
			if err := os.Truncate(filepath.Join(root, "sub", "file.txt"), MaxBytes+1); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, root := workspaceFixture(t)
			tc.mutate(t, root)
			if _, err := Scan(t.Context(), sandbox, "task", "tenant"); !errors.Is(err, tc.expected) {
				t.Fatal(err)
			}
		})
	}
	for _, name := range []string{".", "../outside", "/absolute", "a/../b", "a//b", "a\\b", "C:drive", "a\x00b"} {
		if ValidPath(name) {
			t.Fatal("unsafe path accepted", name)
		}
	}
}

type readBoundaryContext struct {
	context.Context
	calls int
	once  sync.Once
	act   func()
}

func (c *readBoundaryContext) Err() error {
	c.calls++
	if c.calls == 2 {
		c.once.Do(c.act)
	}
	return c.Context.Err()
}

func TestWorkspaceMutationAndCancellationDuringRead(t *testing.T) {
	for _, mode := range []string{"mutation", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			_, path := workspaceFixture(t)
			leaf := filepath.Join(path, "sub", "file.txt")
			if err := os.WriteFile(leaf, make([]byte, 128<<10), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(filepath.Join(path, "sub"))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			before, err := root.Lstat("file.txt")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			boundary := &readBoundaryContext{Context: ctx, act: func() {
				if mode == "cancellation" {
					cancel()
				} else if err := os.WriteFile(leaf, make([]byte, 128<<10), 0600); err != nil {
					t.Fatal(err)
				}
			}}
			scanner := scanner{ctx: boundary}
			err = scanner.readFile(root, "file.txt", "file.txt", before)
			if mode == "mutation" && !errors.Is(err, ErrChanged) {
				t.Fatal("mid-read mutation accepted", err)
			}
			if mode == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatal("mid-read cancellation ignored", err)
			}
		})
	}
}
