package authorityledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Ledger, string) {
	t.Helper()
	ledger, dir, err := Create(t.Context(), t.TempDir(), digest([]byte("namespace")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	return ledger, dir
}
func mutation(label, operation string, epoch int64, incarnation, content string) Mutation {
	hash := ""
	if operation != "tombstone" {
		hash = digest([]byte(content))
	}
	return Mutation{EventID: digest([]byte(label)), TenantID: "tenant-a", Kind: "task", ResourceID: "task-a", Operation: operation, ExpectedEpoch: epoch, ExpectedIncarnation: incarnation, ContentSHA256: hash}
}
func apply(t *testing.T, l *Ledger, m Mutation) *Receipt {
	t.Helper()
	receipt, err := l.Apply(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
func snapshot(t *testing.T, l *Ledger) *Snapshot {
	t.Helper()
	s, err := l.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func floorFixture(t *testing.T, source Identity) (*Floor, string) {
	t.Helper()
	f, dir, err := CreateFloor(t.Context(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, dir
}

func TestLedgerDurableABAAndTombstones(t *testing.T) {
	l, dir := fixture(t)
	first := apply(t, l, mutation("create", "create", 0, "", "A"))
	inc := first.Event.Resource.Incarnation
	a := snapshot(t, l)
	apply(t, l, mutation("B", "update", 1, inc, "B"))
	apply(t, l, mutation("A-again", "update", 2, inc, "A"))
	restored := snapshot(t, l)
	if restored.Resources[0].ContentSHA256 != a.Resources[0].ContentSHA256 || restored.Epoch != 3 || restored.HeadSHA256 == a.HeadSHA256 || restored.Resources[0].Epoch != 3 {
		t.Fatal("ABA history lost")
	}
	apply(t, l, mutation("delete", "tombstone", 3, inc, ""))
	deleted := snapshot(t, l)
	if !deleted.Resources[0].Tombstone || deleted.Resources[0].Incarnation != inc {
		t.Fatal("tombstone lost")
	}
	recreated := apply(t, l, mutation("recreate", "create", 4, inc, "A"))
	if recreated.Event.Resource.Incarnation == inc || recreated.Event.Resource.Epoch != 5 {
		t.Fatal("incarnation reused")
	}
	final := snapshot(t, l)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "ledger.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	ro, err := OpenReadOnly(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got := snapshot(t, ro); !reflect.DeepEqual(final, got) {
		t.Fatal("reopened records differ")
	}
	if _, err := ro.Apply(t.Context(), mutation("write", "invalidate", 5, recreated.Event.Resource.Incarnation, "A")); !errors.Is(err, ErrReadOnly) {
		t.Fatal("read-only write accepted", err)
	}
	if _, err := ro.file.db.Exec("UPDATE metadata SET epoch=epoch"); err == nil {
		t.Fatal("connection is writable")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ledger.sqlite"))
	afterEntries, _ := os.ReadDir(dir)
	if !bytes.Equal(before, after) || len(entries) != len(afterEntries) {
		t.Fatal("read-only reopen changed files")
	}
}

func TestLedgerReplayAndFailedTransactions(t *testing.T) {
	l, _ := fixture(t)
	create := mutation("create", "create", 0, "", "A")
	original := apply(t, l, create)
	before := snapshot(t, l)
	replay := apply(t, l, create)
	if !replay.Replayed || replay.Event != original.Event || !reflect.DeepEqual(before, snapshot(t, l)) {
		t.Fatal("replay reapplied")
	}
	conflict := create
	conflict.ContentSHA256 = digest([]byte("different"))
	if _, err := l.Apply(t.Context(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	m := mutation("update", "update", 1, original.Event.Resource.Incarnation, "B")
	tx, err := l.file.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.applyTx(t.Context(), tx, m); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("INSERT INTO metadata SELECT * FROM metadata"); err == nil {
		t.Fatal("fixture did not force statement failure")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot(t, l)) {
		t.Fatal("failed transaction leaked rows/epochs")
	}
	ctx, cancel := context.WithCancel(t.Context())
	tx, err = l.file.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.applyTx(ctx, tx, m); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = tx.Commit(); err == nil {
		t.Fatal("canceled transaction committed")
	}
	// database/sql rolls a canceled transaction back asynchronously. Wait until
	// its sole connection is returned before checking the preserved fixture.
	connection, err := l.file.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if !reflect.DeepEqual(before, snapshot(t, l)) {
		t.Fatal("canceled transaction leaked")
	}
	// The same event ID is usable after rollback: no phantom commit was published.
	result := apply(t, l, m)
	if result.Replayed || result.Event.Epoch != 2 {
		t.Fatal("rollback retained event")
	}
}

func TestLedgerConcurrentCASAndCancellation(t *testing.T) {
	l, _ := fixture(t)
	initial := apply(t, l, mutation("initial", "create", 0, "", "A"))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"B", "C"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.Apply(t.Context(), mutation(name, "update", 1, initial.Event.Resource.Incarnation, name))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 || snapshot(t, l).Epoch != 2 {
		t.Fatal("CAS lost update")
	}
	if err := l.file.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := l.Snapshot(ctx)
	l.file.unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("contention ignored cancellation", err)
	}
}

func TestIndependentFloorRollbackForkAndReopen(t *testing.T) {
	l, dir := fixture(t)
	f, floorDir := floorFixture(t, l.Identity())
	initial := snapshot(t, l)
	if err := f.Matches(t.Context(), *initial); !errors.Is(err, ErrUnconfirmed) {
		t.Fatal("initial implicit confirmation", err)
	}
	if err := f.Confirm(t.Context(), *initial); err != nil {
		t.Fatal(err)
	}
	first := apply(t, l, mutation("create", "create", 0, "", "A"))
	inc := first.Event.Resource.Incarnation
	old := snapshot(t, l)
	// Keep a dedicated old source image; no original directory or file is replaced.
	backupParent := t.TempDir()
	backupDir := filepath.Join(backupParent, "source-backup")
	if err := os.Mkdir(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	backupDir, err := filepath.EvalSymlinks(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ledger.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(backupDir, "ledger.sqlite"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.Confirm(t.Context(), *old); err != nil {
		t.Fatal(err)
	}
	apply(t, l, mutation("update", "update", 1, inc, "B"))
	current := snapshot(t, l)
	if err = f.Matches(t.Context(), *current); !errors.Is(err, ErrUnconfirmed) {
		t.Fatal("ahead auto-confirmed", err)
	}
	if err = f.Confirm(t.Context(), *current); err != nil {
		t.Fatal(err)
	}
	if err = f.Confirm(t.Context(), *old); !errors.Is(err, ErrRollback) {
		t.Fatal("floor regressed", err)
	}
	forked, err := OpenReadOnly(t.Context(), backupDir)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack := snapshot(t, forked)
	if err = f.Matches(t.Context(), *rolledBack); !errors.Is(err, ErrRollback) {
		t.Fatal("old DB copy accepted", err)
	}
	// A test-only writable handle to this copied fixture models rollback + regrowth.
	forked.Close()
	db, err := openFile(t.Context(), filepath.Join(backupDir, "ledger.sqlite"), "rw")
	if err != nil {
		t.Fatal(err)
	}
	forked.file.db = db
	forked.readOnly = false
	defer forked.Close()
	apply(t, forked, mutation("fork-update", "update", 1, inc, "C"))
	apply(t, forked, mutation("fork-higher", "update", 2, inc, "D"))
	higherFork := snapshot(t, forked)
	if higherFork.Epoch <= current.Epoch {
		t.Fatal("invalid fork fixture")
	}
	if err = f.Confirm(t.Context(), *higherFork); !errors.Is(err, ErrConflict) {
		t.Fatal("higher fork replaced floor", err)
	}
	// Same epoch with a valid but different event chain also fails.
	other := *higherFork
	other.Epoch = 2
	other.Events = append([]Event(nil), higherFork.Events[:2]...)
	other.HeadSHA256 = other.Events[1].SHA256
	other.Resources = []Resource{other.Events[1].Resource}
	if err = f.Confirm(t.Context(), other); !errors.Is(err, ErrConflict) {
		t.Fatal("same epoch fork accepted", err)
	}
	state, err := f.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	before, _ := os.ReadFile(filepath.Join(floorDir, "floor.sqlite"))
	ro, err := OpenFloorReadOnly(t.Context(), floorDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	reopened, err := ro.State(t.Context())
	if err != nil || !reflect.DeepEqual(state, reopened) {
		t.Fatal("floor lost on reopen", err)
	}
	if err = ro.Matches(t.Context(), *current); err != nil {
		t.Fatal(err)
	}
	if err = ro.Confirm(t.Context(), *current); !errors.Is(err, ErrReadOnly) {
		t.Fatal("floor reopened writable", err)
	}
	after, _ := os.ReadFile(filepath.Join(floorDir, "floor.sqlite"))
	if !bytes.Equal(before, after) {
		t.Fatal("floor read modified bytes")
	}
	foreign, _ := fixture(t)
	if err = ro.Matches(t.Context(), *snapshot(t, foreign)); !errors.Is(err, ErrConflict) {
		t.Fatal("origin replacement accepted", err)
	}
}

func TestLedgerValidationBudgetsAndCorruption(t *testing.T) {
	for _, scenario := range []string{"unknown_kind", "empty_tenant", "bad_digest", "bad_expected", "noop", "overflow", "unknown_table", "bad_body", "oversize_body", "event_budget", "truncated_history"} {
		t.Run(scenario, func(t *testing.T) {
			l, _ := fixture(t)
			created := apply(t, l, mutation("create", "create", 0, "", "A"))
			before := snapshot(t, l)
			m := mutation("update", "update", 1, created.Event.Resource.Incarnation, "B")
			switch scenario {
			case "unknown_kind":
				m.Kind = "unknown"
			case "empty_tenant":
				m.TenantID = ""
			case "bad_digest":
				m.ContentSHA256 = "bad"
			case "bad_expected":
				m.ExpectedEpoch = 0
				m.ExpectedIncarnation = ""
			case "noop":
				m.ContentSHA256 = created.Event.Resource.ContentSHA256
			case "overflow":
				previous := created.Event.Resource
				previous.Epoch = math.MaxInt64
				m.ExpectedEpoch = math.MaxInt64
				if _, err := transition(l.Identity(), 2, previous, true, m); !errors.Is(err, ErrBudget) {
					t.Fatal("overflow allowed", err)
				}
				return
			case "unknown_table":
				l.file.db.Exec("CREATE TABLE foreign_data(id INTEGER)")
			case "bad_body":
				l.file.db.Exec("UPDATE events SET body=?", []byte(`{"epoch":"1","epoch":"1"}`))
			case "oversize_body":
				l.file.db.Exec("UPDATE events SET body=?", bytes.Repeat([]byte("x"), maxBodyBytes+1))
			case "event_budget":
				tx, err := l.file.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				for i := int64(2); i <= MaxEvents+1; i++ {
					if _, err = tx.Exec("INSERT INTO events VALUES(?,?,?)", i, digest([]byte(string(rune(i)))), []byte("{}")); err != nil {
						t.Fatal(err)
					}
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			case "truncated_history":
				l.file.db.Exec("DELETE FROM events WHERE epoch=1")
			}
			_, err := l.Apply(t.Context(), m)
			if err == nil {
				t.Fatal("invalid mutation/store accepted")
			}
			if scenario == "oversize_body" || scenario == "event_budget" {
				if !errors.Is(err, ErrBudget) {
					t.Fatal("lost budget error", err)
				}
			}
			if scenario == "unknown_kind" || scenario == "empty_tenant" || scenario == "bad_digest" || scenario == "bad_expected" || scenario == "noop" {
				if !reflect.DeepEqual(before, snapshot(t, l)) {
					t.Fatal("denial changed source")
				}
			}
		})
	}
}

func TestLedgerPrivateFormatAndHotJournalRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and symlink fixture")
	}
	for _, scenario := range []string{"symlink", "permission", "journal", "wrong_marker", "foreign_format"} {
		t.Run(scenario, func(t *testing.T) {
			l, dir := fixture(t)
			l.Close()
			path := filepath.Join(dir, "ledger.sqlite")
			switch scenario {
			case "symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "permission":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if err := os.WriteFile(path+"-journal", []byte("preserved-journal"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong_marker":
				db, err := openFile(t.Context(), path, "rw")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("PRAGMA application_id=1"); err != nil {
					t.Fatal(err)
				}
				db.Close()
			case "foreign_format":
				db, err := openFile(t.Context(), path, "rw")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("CREATE TABLE foreign_data(secret TEXT)"); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			before, _ := os.ReadFile(path)
			entries, _ := os.ReadDir(filepath.Dir(path))
			opened, err := OpenReadOnly(t.Context(), dir)
			if err == nil || opened != nil {
				t.Fatal("unsafe fixture accepted")
			}
			after, _ := os.ReadFile(path)
			afterEntries, _ := os.ReadDir(filepath.Dir(path))
			if !bytes.Equal(before, after) || len(entries) != len(afterEntries) {
				t.Fatal("denial repaired or changed source")
			}
		})
	}
}

func TestLedgerProcessExitHelper(t *testing.T) {
	mode := os.Getenv("AI_AGENT_C01_EXIT_MODE")
	if mode == "" {
		return
	}
	dir := os.Getenv("AI_AGENT_C01_EXIT_DIRECTORY")
	l, err := OpenReadOnly(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	db, err := openFile(t.Context(), filepath.Join(dir, "ledger.sqlite"), "rw")
	if err != nil {
		t.Fatal(err)
	}
	l.file.db = db
	l.readOnly = false
	if mode == "before_commit" {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = l.applyTx(t.Context(), tx, mutation("process-create", "create", 0, "", "A")); err != nil {
			t.Fatal(err)
		}
		os.Exit(23)
	}
	if mode != "after_commit" {
		t.Fatal("invalid helper mode")
	}
	if _, err = l.Apply(t.Context(), mutation("process-create", "create", 0, "", "A")); err != nil {
		t.Fatal(err)
	}
	os.Exit(23)
}
func TestLedgerProcessExitCommitBoundary(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			l, dir := fixture(t)
			l.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLedgerProcessExitHelper$", "-test.count=1")
			command.Env = append(os.Environ(), "AI_AGENT_C01_EXIT_MODE="+mode, "AI_AGENT_C01_EXIT_DIRECTORY="+dir)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("helper did not reach exit boundary: %v %s", err, output)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "ledger.sqlite"))
			entries, _ := os.ReadDir(dir)
			ro, err := OpenReadOnly(t.Context(), dir)
			if mode == "before_commit" {
				// An unfinished journal is preserved and rejected, never repaired as a read.
				if err == nil {
					defer ro.Close()
					if snapshot(t, ro).Epoch != 0 {
						t.Fatal("uncommitted event visible")
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer ro.Close()
				if snapshot(t, ro).Epoch != 1 {
					t.Fatal("committed event missing")
				}
			}
			after, _ := os.ReadFile(filepath.Join(dir, "ledger.sqlite"))
			afterEntries, _ := os.ReadDir(dir)
			if !bytes.Equal(before, after) || len(entries) != len(afterEntries) {
				t.Fatal("post-exit read changed evidence")
			}
		})
	}
}

func TestLedgerSnapshotOwnershipAndFloorValidation(t *testing.T) {
	l, _ := fixture(t)
	apply(t, l, mutation("create", "create", 0, "", "A"))
	original := snapshot(t, l)
	data, _ := json.Marshal(original)
	var copy Snapshot
	json.Unmarshal(data, &copy)
	copy.Resources[0].ContentSHA256 = digest([]byte("forged"))
	if _, err := SnapshotDigest(copy); !errors.Is(err, ErrInvalid) {
		t.Fatal("forged materialized resource accepted", err)
	}
	original.Events[0].Mutation.ResourceID = "mutated"
	if _, err := SnapshotDigest(*original); !errors.Is(err, ErrInvalid) {
		t.Fatal("forged history accepted", err)
	}
	next := snapshot(t, l)
	if next.Events[0].Mutation.ResourceID != "task-a" {
		t.Fatal("returned history aliases Store")
	}
	f, _ := floorFixture(t, l.Identity())
	if err := f.Confirm(t.Context(), *next); err != nil {
		t.Fatal(err)
	}
	if err := f.Confirm(t.Context(), *next); err != nil {
		t.Fatal("identical floor retry failed", err)
	}
}

func TestLedgerInvalidateOldReplayAndScopeIsolation(t *testing.T) {
	l, _ := fixture(t)
	create := mutation("create", "create", 0, "", "A")
	first := apply(t, l, create)
	inc := first.Event.Resource.Incarnation
	invalidated := apply(t, l, mutation("explicit-invalidation", "invalidate", 1, inc, "A"))
	if invalidated.Event.Resource.ContentSHA256 != first.Event.Resource.ContentSHA256 || invalidated.Event.Resource.Epoch != 2 || invalidated.Event.SHA256 == first.Event.SHA256 {
		t.Fatal("explicit invalidation reused version")
	}
	wrongTenant := mutation("foreign-update", "update", 2, inc, "B")
	wrongTenant.TenantID = "tenant-b"
	if _, err := l.Apply(t.Context(), wrongTenant); !errors.Is(err, ErrConflict) {
		t.Fatal("foreign scope used source incarnation", err)
	}
	before := snapshot(t, l)
	replayed := apply(t, l, create)
	if !replayed.Replayed || replayed.Event != first.Event || !reflect.DeepEqual(before, snapshot(t, l)) {
		t.Fatal("old retry mutated current head")
	}
	stale := mutation("stale-after-retry", "update", replayed.Event.Resource.Epoch, replayed.Event.Resource.Incarnation, "B")
	if _, err := l.Apply(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatal("original receipt masqueraded as current head", err)
	}
}

func TestLedgerBoundedLabelsAndFloorRecords(t *testing.T) {
	for _, scenario := range []string{"metadata", "event_label", "floor_identity", "floor_digest"} {
		t.Run(scenario, func(t *testing.T) {
			l, _ := fixture(t)
			apply(t, l, mutation("create", "create", 0, "", "A"))
			switch scenario {
			case "metadata":
				if _, err := l.file.db.Exec("UPDATE metadata SET origin=?", string(bytes.Repeat([]byte("a"), 1024))); err != nil {
					t.Fatal(err)
				}
				if _, err := l.Snapshot(t.Context()); err == nil {
					t.Fatal("oversize metadata accepted")
				}
			case "event_label":
				if _, err := l.file.db.Exec("UPDATE events SET event_id=?", string(bytes.Repeat([]byte("a"), 1024))); err != nil {
					t.Fatal(err)
				}
				if _, err := l.Snapshot(t.Context()); !errors.Is(err, ErrBudget) {
					t.Fatal("oversize label accepted", err)
				}
			case "floor_identity", "floor_digest":
				f, dir := floorFixture(t, l.Identity())
				if scenario == "floor_identity" {
					f.file.db.Exec("UPDATE floor_state SET source_identity=?", bytes.Repeat([]byte("a"), maxBodyBytes+1))
				} else {
					f.file.db.Exec("UPDATE floor_state SET head_sha256=?", string(bytes.Repeat([]byte("a"), 1024)))
				}
				if _, err := f.State(t.Context()); err == nil {
					t.Fatal("oversize floor accepted")
				}
				f.Close()
				if opened, err := OpenFloorReadOnly(t.Context(), dir); err == nil || opened != nil {
					t.Fatal("oversize floor reopened")
				}
			}
		})
	}
}
