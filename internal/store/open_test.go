package store

import (
	"path/filepath"
	"testing"
)

func TestOpenMemory(t *testing.T) {
	st, err := Open("memory", "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*MemoryStore); !ok {
		t.Fatalf("Open(memory) = %T, want *MemoryStore", st)
	}
}

func TestOpenSQLite(t *testing.T) {
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*SQLiteStore); !ok {
		t.Fatalf("Open(sqlite) = %T, want *SQLiteStore", st)
	}
}

func TestOpenExternalRequiresDSN(t *testing.T) {
	for _, kind := range []string{"postgres", "redis"} {
		t.Run(kind, func(t *testing.T) {
			st, err := Open(kind, "")
			if err == nil || st != nil {
				t.Fatalf("Open(%q) = %T, %v; want error", kind, st, err)
			}
		})
	}
}

func TestOpenUnknownDefaultsToSQLite(t *testing.T) {
	st, err := Open("", filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*SQLiteStore); !ok {
		t.Fatalf("Open(unknown) = %T, want *SQLiteStore", st)
	}
}

func TestOpenPreservesExactKindSemantics(t *testing.T) {
	st, err := Open(" MEMORY ", filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok := st.(*SQLiteStore); !ok {
		t.Fatalf("Open(\" MEMORY \") = %T, want *SQLiteStore", st)
	}
}
