package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReadFileBoundedUTF8AndRegularFiles(t *testing.T) {
	workspace := t.TempDir()
	for _, tc := range []struct{ name, body, want string }{
		{"short", "hello", "hello"},
		{"multibyte", strings.Repeat("中", 2000), strings.Repeat("中", 1333)},
		{"invalid", "a\xffb", "a\ufffdb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(workspace, tc.name), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadFile(workspace, tc.name)
			if err != nil || got != tc.want || len(got) > 4000 || !utf8.ValidString(got) {
				t.Fatalf("read len=%d valid=%v err=%v", len(got), utf8.ValidString(got), err)
			}
		})
	}
	if _, err := ReadFile(workspace, "."); err == nil {
		t.Fatal("directory accepted")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err == nil {
		if _, err := ReadFile(workspace, "escape/secret"); err == nil {
			t.Fatal("outside symlink accepted")
		}
	}
}

func TestReadFileLargeInputDoesNotAllocateWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := ReadFile(filepath.Dir(path), "large")
	runtime.ReadMemStats(&after)
	if err != nil || len(got) != 4000 {
		t.Fatal("bounded read", len(got), err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Fatalf("read 4000 bytes allocated %d bytes for 8 MiB file", allocated)
	}
}

func TestReadFileToolCanceledDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&ReadFileTool{}).Execute(ctx, "missing-workspace", map[string]any{"path": "missing"})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read ignored context", err)
	}
}
