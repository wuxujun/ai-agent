package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSessionCreationDistinguishesConflict(t *testing.T) {
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for _, st := range []Store{NewMemoryStore(), sqlite} {
		sessions := st.(SessionStore)
		first := &types.Session{ID: "same", TenantID: "tenant-a", Title: "original"}
		if err := sessions.CreateSession(t.Context(), first); err != nil {
			t.Fatal(err)
		}
		if err := sessions.CreateSession(t.Context(), &types.Session{ID: "same", TenantID: "tenant-b", Title: "replacement"}); !errors.Is(err, ErrSessionExists) {
			t.Fatalf("%T conflict: %v", st, err)
		}
		loaded, err := sessions.GetSession(t.Context(), "same", "tenant-a")
		if err != nil || loaded.Title != "original" {
			t.Fatalf("duplicate modified original: %+v %v", loaded, err)
		}
	}
	if err := sqlite.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.CreateSession(t.Context(), &types.Session{ID: "new"}); err == nil || errors.Is(err, ErrSessionExists) {
		t.Fatalf("backend failure misclassified: %v", err)
	}
}
