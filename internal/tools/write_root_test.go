package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileRootRejectsSwappedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege dependent")
	}
	workspace, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(filepath.Join(workspace, "sub"), filepath.Join(workspace, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileInRoot(root, "sub/target.txt", "changed"); err == nil {
		t.Fatal("followed swapped parent")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "original" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
}

func TestWriteFileCreatesParentsAndTruncatesRegularFile(t *testing.T) {
	workspace := t.TempDir()
	for _, content := range []string{"long original content", "short"} {
		if err := WriteFile(workspace, "nested/sub/file.txt", content); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(workspace, "nested/sub/file.txt"))
		if err != nil || string(data) != content {
			t.Fatalf("data=%q err=%v", data, err)
		}
	}
}
