package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type sqliteStartupAnchorFunc func(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error)

func (f sqliteStartupAnchorFunc) ReadSQLiteRestoreStartupAnchor(ctx context.Context, scope SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
	return f(ctx, scope)
}

func sqliteStartupFixture(t *testing.T) (*SQLiteStore, SQLiteRestoreStartupRequest, SQLiteRestoreStartupAnchor) {
	t.Helper()
	st := restoreVersionStore(t)
	restoreVersionTask(t, st)
	observation := restoreVersionRead(t, st)
	q := SQLiteRestoreStartupRequest{Pin: restoreVersionPin(observation), AuthorityIdentitySHA256: strings.Repeat("d", 64)}
	anchor := SQLiteRestoreStartupAnchor{Scope: SQLiteRestoreStartupScope{SchemaSHA256: observation.SchemaSHA256, Origin: observation.Origin, NamespaceSHA256: observation.NamespaceSHA256, Coverage: sqliteRestoreVersionCoverage, ManifestSHA256: sqliteRestoreStartupManifest(), AuthorityIdentitySHA256: q.AuthorityIdentitySHA256}, Epoch: observation.Epoch, EventsSHA256: observation.EventsSHA256, DataSHA256: observation.DataSHA256, GenerationSHA256: strings.Repeat("b", 64), WriterIdentitySHA256: strings.Repeat("c", 64), WriterPolicy: sqliteRestoreWriterPolicy, Status: "active", IssuedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Minute)}
	return st, q, anchor
}

func TestSQLiteRestoreStartupReadOnlyContract(t *testing.T) {
	st, q, anchor := sqliteStartupFixture(t)
	if _, err := st.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	before := restoreVersionRead(t, st)
	fileBefore, err := os.ReadFile(st.restoreVersions.path)
	if err != nil {
		t.Fatal(err)
	}
	var changes int64
	if err := st.db.QueryRow(`SELECT total_changes()`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	calls := 0
	q.AnchorReader = sqliteStartupAnchorFunc(func(_ context.Context, scope SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
		calls++
		if scope != anchor.Scope {
			t.Fatal("wrong request scope")
		}
		return anchor, nil
	})
	out, err := st.ReadSQLiteRestoreStartup(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Status != "unverified" || !out.AnchorContractMatched || len(out.Gaps) != 4 || out.Pin != q.Pin {
		t.Fatal("startup contract result")
	}
	after := restoreVersionRead(t, st)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("startup changed contents")
	}
	fileAfter, err := os.ReadFile(st.restoreVersions.path)
	if err != nil {
		t.Fatal(err)
	}
	var finalChanges int64
	if err := st.db.QueryRow(`SELECT total_changes()`).Scan(&finalChanges); err != nil {
		t.Fatal(err)
	}
	if changes != finalChanges || !bytes.Equal(fileBefore, fileAfter) {
		t.Fatal("startup wrote")
	}
	report, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(report, []byte("secret-goal")) {
		t.Fatal("body leaked")
	}
	q.AnchorReader = nil
	local, err := st.ReadSQLiteRestoreStartup(t.Context(), q)
	if err != nil || local.AnchorContractMatched || len(local.Gaps) != 5 || local.Status != "unverified" {
		t.Fatal("missing anchor was trusted", err)
	}
}

func TestSQLiteRestoreStartupRejectsAnchorContract(t *testing.T) {
	scenarios := []struct {
		name   string
		change func(*SQLiteRestoreStartupAnchor)
	}{
		{"schema", func(a *SQLiteRestoreStartupAnchor) { a.Scope.SchemaSHA256 = strings.Repeat("e", 64) }},
		{"origin", func(a *SQLiteRestoreStartupAnchor) { a.Scope.Origin = strings.Repeat("e", 64) }},
		{"namespace", func(a *SQLiteRestoreStartupAnchor) { a.Scope.NamespaceSHA256 = strings.Repeat("e", 64) }},
		{"coverage", func(a *SQLiteRestoreStartupAnchor) { a.Scope.Coverage = "unknown" }},
		{"manifest", func(a *SQLiteRestoreStartupAnchor) { a.Scope.ManifestSHA256 = strings.Repeat("e", 64) }},
		{"authority", func(a *SQLiteRestoreStartupAnchor) { a.Scope.AuthorityIdentitySHA256 = strings.Repeat("e", 64) }},
		{"floor_behind", func(a *SQLiteRestoreStartupAnchor) { a.Epoch-- }},
		{"floor_ahead", func(a *SQLiteRestoreStartupAnchor) { a.Epoch++ }},
		{"events", func(a *SQLiteRestoreStartupAnchor) { a.EventsSHA256 = strings.Repeat("e", 64) }},
		{"data", func(a *SQLiteRestoreStartupAnchor) { a.DataSHA256 = strings.Repeat("e", 64) }},
		{"generation", func(a *SQLiteRestoreStartupAnchor) { a.GenerationSHA256 = "invalid" }},
		{"writer_identity", func(a *SQLiteRestoreStartupAnchor) { a.WriterIdentitySHA256 = "invalid" }},
		{"writer_policy", func(a *SQLiteRestoreStartupAnchor) { a.WriterPolicy = "shared-writers" }},
		{"revoked", func(a *SQLiteRestoreStartupAnchor) { a.Status = "revoked" }},
		{"future", func(a *SQLiteRestoreStartupAnchor) { a.IssuedAt = time.Now().UTC().Add(time.Second) }},
		{"expired", func(a *SQLiteRestoreStartupAnchor) { a.ExpiresAt = time.Now().UTC().Add(-time.Second) }},
		{"long_window", func(a *SQLiteRestoreStartupAnchor) { a.ExpiresAt = a.IssuedAt.Add(6 * time.Minute) }},
		{"empty_window", func(a *SQLiteRestoreStartupAnchor) { a.ExpiresAt = a.IssuedAt }},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			st, q, anchor := sqliteStartupFixture(t)
			scenario.change(&anchor)
			q.AnchorReader = sqliteStartupAnchorFunc(func(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
				return anchor, nil
			})
			if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
				t.Fatal("accepted", err)
			}
		})
	}
}

func TestSQLiteRestoreStartupBeforeAfterChanges(t *testing.T) {
	for _, change := range []string{"generation", "writer", "revocation", "window", "database"} {
		t.Run(change, func(t *testing.T) {
			st, q, anchor := sqliteStartupFixture(t)
			calls := 0
			q.AnchorReader = sqliteStartupAnchorFunc(func(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
				calls++
				after := anchor
				if calls == 2 {
					switch change {
					case "generation":
						after.GenerationSHA256 = strings.Repeat("e", 64)
					case "writer":
						after.WriterIdentitySHA256 = strings.Repeat("e", 64)
					case "revocation":
						after.Status = "revoked"
					case "window":
						after.ExpiresAt = after.ExpiresAt.Add(time.Second)
					case "database":
						if _, err := st.db.Exec(`UPDATE tasks SET goal='changed'`); err != nil {
							t.Fatal(err)
						}
					}
				}
				return after, nil
			})
			if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
				t.Fatal("change accepted", err)
			}
		})
	}
}

func TestSQLiteRestoreStartupProfileAndPin(t *testing.T) {
	for _, query := range []string{`PRAGMA synchronous=OFF`, `PRAGMA trusted_schema=OFF`, `PRAGMA recursive_triggers=ON`, `PRAGMA journal_mode=WAL`, `DROP TRIGGER restore_row_tasks_update`} {
		t.Run(query, func(t *testing.T) {
			st, q, _ := sqliteStartupFixture(t)
			if _, err := st.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
				t.Fatal("unsafe profile accepted", err)
			}
		})
	}
	// Even a caller-supplied/schema-self pin cannot make a missing expected
	// recording trigger pass the code-defined startup coverage profile.
	st, q, _ := sqliteStartupFixture(t)
	if _, err := st.db.Exec(`DROP TRIGGER restore_row_tasks_update`); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := sqliteRestoreVersionSchema(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	st.restoreVersions.schema = schema
	q.Pin.SchemaSHA256 = schema
	if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("self-pin bypassed expected coverage", err)
	}
	st, q, _ = sqliteStartupFixture(t)
	q.Pin.MinimumEpoch++
	if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("old source accepted", err)
	}
	q.Pin.MinimumEpoch--
	q.Pin.PrefixEventsSHA256 = strings.Repeat("e", 64)
	if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("wrong history prefix accepted", err)
	}
}

func TestSQLiteRestoreStartupFailureCancellationAndBounds(t *testing.T) {
	st, q, _ := sqliteStartupFixture(t)
	q.AnchorReader = sqliteStartupAnchorFunc(func(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
		return SQLiteRestoreStartupAnchor{}, errors.New("secret-upstream-body")
	})
	if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) || strings.Contains(err.Error(), "secret") {
		t.Fatal("unsanitized failure")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.ReadSQLiteRestoreStartup(ctx, q); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	q.AnchorReader = nil
	q.Pin.Origin = strings.Repeat("a", 1<<20)
	if _, err := st.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsInvalid) {
		t.Fatal("oversized pin accepted")
	}
	if sqliteRestoreContentValidDigest(strings.Repeat("a", 1<<20)) {
		t.Fatal("oversized digest accepted")
	}
	if sqliteRestoreStartupManifest() == sqliteRestoreVersionHash(struct{}{}) {
		t.Fatal("empty manifest")
	}
}

func TestSQLiteRestoreStartupReadOnlyReopen(t *testing.T) {
	st, q, anchor := sqliteStartupFixture(t)
	path := st.restoreVersions.path
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenSQLiteRestoreVersionReader(path, q.Pin)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	provider := sqliteStartupAnchorFunc(func(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error) {
		return anchor, nil
	})
	out, err := reader.ReadSQLiteRestoreStartup(t.Context(), q.AuthorityIdentitySHA256, provider)
	if err != nil || !out.AnchorContractMatched || out.Status != "unverified" {
		t.Fatal("reopened startup", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reopen startup modified file", err)
	}
	plain := localObservationStore(t, "sqlite").(*SQLiteStore)
	if _, err := plain.ReadSQLiteRestoreStartup(t.Context(), q); !errors.Is(err, ErrSQLiteRestoreVersionsUnavailable) {
		t.Fatal("default store enabled startup profile", err)
	}
}
