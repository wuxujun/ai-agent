package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestSQLiteRestoreContentHashesAndHiddenWrite(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	before := restoreVersionRead(t, st)
	if _, err := st.db.Exec(`UPDATE tasks SET llm_cost_budget_usd=? WHERE id='task'`, 1.0); err != nil {
		t.Fatal(err)
	}
	one := restoreVersionRead(t, st)
	if _, err := st.db.Exec(`UPDATE tasks SET llm_cost_budget_usd=? WHERE id='task'`, math.Nextafter(1, 2)); err != nil {
		t.Fatal(err)
	}
	next := restoreVersionRead(t, st)
	if restoreVersionFind(t, one, "task", "task").ContentSHA256 == restoreVersionFind(t, next, "task", "task").ContentSHA256 {
		t.Fatal("adjacent doubles have identical digest")
	}
	if before.EventsSHA256 == next.EventsSHA256 {
		t.Fatal("content history not bound")
	}
	// Remove then recreate byte-identical trigger: schema fingerprint alone can
	// no longer see the temporary removal, but current content must still match.
	var ddl string
	if err := st.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='restore_row_tasks_update'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER restore_row_tasks_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE tasks SET goal='unrecorded'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadSQLiteRestoreVersions(t.Context()); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("unrecorded content accepted", err)
	}
	if _, err := st.db.Exec(`UPDATE tasks SET goal='new-tracked-write'`); err == nil {
		t.Fatal("dirty predecessor acknowledged")
	}
}

func TestSQLiteRestoreContentHistoricalForkPin(t *testing.T) {
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	if _, err := st.db.Exec(`UPDATE tasks SET goal='B';UPDATE tasks SET goal='secret-goal'`); err != nil {
		t.Fatal(err)
	}
	before := restoreVersionRead(t, st)
	pin := restoreVersionPin(before)
	if len(before.Events) < 3 {
		t.Fatal("unexpected fixture event count")
	}
	// Rewrite only an intermediate content hash, retaining valid continuity,
	// identical operation/identity history, and identical current business data.
	if _, err := st.db.Exec(`UPDATE restore_row_events SET after_sha256=? WHERE epoch=?`, strings.Repeat("0", 64), before.Epoch-1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE restore_row_events SET before_sha256=? WHERE epoch=?`, strings.Repeat("0", 64), before.Epoch); err != nil {
		t.Fatal(err)
	}

	after := restoreVersionRead(t, st)
	if before.DataSHA256 != after.DataSHA256 || before.EventsSHA256 == after.EventsSHA256 {
		t.Fatal("content fork not distinguished")
	}
	path := st.restoreVersions.path
	st.Close()
	if reader, err := OpenSQLiteRestoreVersionReader(path, pin); err == nil {
		reader.Close()
		t.Fatal("same-layout content fork accepted")
	}
}

func TestSQLiteRestoreBaselineCompleteScopedAndReadOnly(t *testing.T) {
	st := restoreVersionStore(t)
	q := seedLocalObservation(t, st, 3)
	if err := st.SaveFullTask(t.Context(), &types.Task{ID: "other", TenantID: "b", Goal: "other-tenant", Status: types.StatusPaused}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	out, err := st.ReadSQLiteRestoreBaseline(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "unverified" || len(out.Session.Members) != 3 || out.Snapshot.Task.ID != q.TaskID || out.Session.HighWatermark != 9007199254740993 {
		t.Fatal("incomplete baseline")
	}
	for _, version := range out.Versions {
		if version.ID == "other" {
			t.Fatal("other tenant scope leaked")
		}
		if !sqliteRestoreContentValidDigest(version.ContentSHA256) {
			t.Fatal("unbound live version")
		}
	}
	if out.Pin.MinimumEpoch < 1 || !sqliteRestoreContentValidDigest(out.Pin.PrefixEventsSHA256) {
		t.Fatal("missing revision pin")
	}
	digest := out.SHA256
	out.SHA256 = ""
	if sqliteRestoreVersionHash(out) != digest {
		t.Fatal("baseline digest not reproducible")
	}
	out.SHA256 = digest
	taskBody, _ := json.Marshal(out.Snapshot.Task)
	traceBody, _ := json.Marshal(out.Snapshot.Traces)
	member := out.Session.Members[0]
	if member.TaskSHA256 != restoreDigest(taskBody) || member.TraceSHA256 != restoreDigest(traceBody) {
		t.Fatal("export and member diverge")
	}
	again, err := st.ReadSQLiteRestoreBaseline(t.Context(), q)
	if err != nil || !reflect.DeepEqual(again, out) {
		t.Fatal("unstable readonly baseline", err)
	}
	wrong := q
	wrong.TenantID = "b"
	if _, err := st.ReadSQLiteRestoreBaseline(t.Context(), wrong); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross tenant accepted")
	}
	tiny := q
	tiny.MaxTasks = 2
	if _, err := st.ReadSQLiteRestoreBaseline(t.Context(), tiny); !errors.Is(err, ErrTaskExportTooLarge) {
		t.Fatal("incomplete members accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.ReadSQLiteRestoreBaseline(ctx, q); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSQLiteRestoreBaselineSingleTransaction(t *testing.T) {
	st := restoreVersionStore(t)
	q := seedLocalObservation(t, st, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	errs := make(chan error, 1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 30; i++ {
			tx, err := st.db.BeginTx(t.Context(), nil)
			if err != nil {
				errs <- err
				return
			}
			if _, err = tx.Exec(`UPDATE tasks SET step_count=?;UPDATE sessions SET next_sequence=?`, i, i); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			if err = tx.Commit(); err != nil {
				errs <- err
				return
			}
		}
	}()
	for i := 0; i < 30; i++ {
		out, err := st.ReadSQLiteRestoreBaseline(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		high := out.Session.HighWatermark
		if high != 9007199254740993 && int64(out.Snapshot.Task.StepCount) != high {
			t.Fatal("mixed export/session transactions")
		}
		root := out.Session.Members[0]
		body, _ := json.Marshal(out.Snapshot.Task)
		if root.TaskSHA256 != restoreDigest(body) {
			t.Fatal("mixed member/export")
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
