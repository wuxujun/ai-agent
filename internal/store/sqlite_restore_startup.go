package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const sqliteRestoreWriterPolicy = "sqlite-private-single-writer-v1"

// SQLiteRestoreStartupAnchorReader is selected by trusted code, never by a
// restore artifact. No runtime/default provider is installed. A provider must
// obtain its own independent floor and protected-writer facts; this prototype
// checks the contract and does not authenticate the provider or enforce fencing.
type SQLiteRestoreStartupAnchorReader interface {
	ReadSQLiteRestoreStartupAnchor(context.Context, SQLiteRestoreStartupScope) (SQLiteRestoreStartupAnchor, error)
}

type SQLiteRestoreStartupScope struct {
	SchemaSHA256            string
	Origin                  string
	NamespaceSHA256         string
	Coverage                string
	ManifestSHA256          string
	AuthorityIdentitySHA256 string
}

type SQLiteRestoreStartupAnchor struct {
	Scope                SQLiteRestoreStartupScope
	Epoch                int64
	EventsSHA256         string
	DataSHA256           string
	GenerationSHA256     string
	WriterIdentitySHA256 string
	WriterPolicy         string
	Status               string
	IssuedAt             time.Time
	ExpiresAt            time.Time
}

type SQLiteRestoreStartupRequest struct {
	Pin                     SQLiteRestoreVersionPin
	AuthorityIdentitySHA256 string
	AnchorReader            SQLiteRestoreStartupAnchorReader
}

// All successful reports remain unverified: a startup read does not fence
// future writes, authenticate an independent authority, or stop executors.
type SQLiteRestoreStartupReport struct {
	Status                 string                  `json:"status"`
	Coverage               string                  `json:"coverage"`
	ManifestSHA256         string                  `json:"manifest_sha256"`
	Pin                    SQLiteRestoreVersionPin `json:"pin"`
	DataSHA256             string                  `json:"data_sha256"`
	AnchorContractMatched  bool                    `json:"anchor_contract_matched"`
	AnchorGenerationSHA256 string                  `json:"anchor_generation_sha256,omitempty"`
	WriterIdentitySHA256   string                  `json:"writer_identity_sha256,omitempty"`
	Gaps                   []string                `json:"gaps"`
}

func sqliteRestoreStartupManifest() string {
	tables := make([]string, len(sqliteRestoreVersionTables))
	for i, table := range sqliteRestoreVersionTables {
		tables[i] = table.name + ":" + table.kind + ":" + table.key
	}
	return sqliteRestoreVersionHash(struct {
		Coverage, WriterPolicy, JournalMode, ContentFunction string
		Synchronous, TrustedSchema, RecursiveTriggers        int
		Tables                                               []string
		MaxEvents, MaxRows, MaxBytes                         int
	}{sqliteRestoreVersionCoverage, sqliteRestoreWriterPolicy, "delete", sqliteRestoreContentFunction, 2, 1, 0, tables, sqliteRestoreVersionMaxEvents, sqliteRestoreVersionMaxRows, sqliteRestoreVersionMaxBytes})
}

// ReadSQLiteRestoreStartup performs read-only startup validation for an
// explicitly created prototype. A current independent anchor is mandatory for
// AnchorContractMatched, and is read twice around database snapshots. Matching
// this fixture contract never admits a writer or enables restoration.
func (s *SQLiteStore) ReadSQLiteRestoreStartup(ctx context.Context, q SQLiteRestoreStartupRequest) (*SQLiteRestoreStartupReport, error) {
	if s.restoreVersions == nil {
		return nil, ErrSQLiteRestoreVersionsUnavailable
	}
	if err := sqliteRestoreStartupValidatePin(q.Pin); err != nil {
		return nil, err
	}
	if q.AnchorReader != nil && !sqliteRestoreContentValidDigest(q.AuthorityIdentitySHA256) {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	scope := SQLiteRestoreStartupScope{SchemaSHA256: q.Pin.SchemaSHA256, Origin: q.Pin.Origin, NamespaceSHA256: q.Pin.NamespaceSHA256, Coverage: sqliteRestoreVersionCoverage, ManifestSHA256: sqliteRestoreStartupManifest(), AuthorityIdentitySHA256: q.AuthorityIdentitySHA256}
	var before SQLiteRestoreStartupAnchor
	var err error
	if q.AnchorReader != nil {
		before, err = q.AnchorReader.ReadSQLiteRestoreStartupAnchor(ctx, scope)
		if err != nil {
			return nil, sqliteRestoreVersionError(ctx, err)
		}
	}
	observation, err := s.readSQLiteRestoreStartupSnapshot(ctx, q.Pin)
	if err != nil {
		return nil, err
	}
	out := &SQLiteRestoreStartupReport{Status: "unverified", Coverage: scope.Coverage, ManifestSHA256: scope.ManifestSHA256, Pin: SQLiteRestoreVersionPin{
		Origin: observation.Origin, NamespaceSHA256: observation.NamespaceSHA256, SchemaSHA256: observation.SchemaSHA256, MinimumEpoch: observation.Epoch, PrefixEventsSHA256: observation.EventsSHA256,
	}, DataSHA256: observation.DataSHA256, Gaps: []string{"authenticated_anchor_transport_unavailable", "runtime_writer_fencing_unimplemented", "other_store_domains_untracked", "execution_isolation_unverified"}}
	if q.AnchorReader == nil {
		out.Gaps = append(out.Gaps, "startup_anchor_unavailable")
		return out, nil
	}
	if err := sqliteRestoreStartupValidateAnchor(before, scope, observation, time.Now().UTC()); err != nil {
		return nil, err
	}
	after, err := q.AnchorReader.ReadSQLiteRestoreStartupAnchor(ctx, scope)
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if before != after {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	current, err := s.readSQLiteRestoreStartupSnapshot(ctx, q.Pin)
	if err != nil {
		return nil, err
	}
	if current.Epoch != observation.Epoch || current.EventsSHA256 != observation.EventsSHA256 || current.DataSHA256 != observation.DataSHA256 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := sqliteRestoreStartupValidateAnchor(after, scope, current, time.Now().UTC()); err != nil {
		return nil, err
	}
	out.AnchorContractMatched = true
	out.AnchorGenerationSHA256 = before.GenerationSHA256
	out.WriterIdentitySHA256 = before.WriterIdentitySHA256
	return out, nil
}

func sqliteRestoreStartupValidatePin(pin SQLiteRestoreVersionPin) error {
	if pin.MinimumEpoch < 0 || pin.MinimumEpoch > sqliteRestoreVersionMaxEvents {
		return ErrSQLiteRestoreVersionsInvalid
	}
	for _, digest := range []string{pin.Origin, pin.NamespaceSHA256, pin.SchemaSHA256, pin.PrefixEventsSHA256} {
		if !sqliteRestoreContentValidDigest(digest) {
			return ErrSQLiteRestoreVersionsInvalid
		}
	}
	return nil
}

func sqliteRestoreStartupValidateAnchor(anchor SQLiteRestoreStartupAnchor, scope SQLiteRestoreStartupScope, observation *SQLiteRestoreVersionObservation, now time.Time) error {
	if anchor.Scope != scope || anchor.Status != "active" || anchor.WriterPolicy != sqliteRestoreWriterPolicy || anchor.Epoch != observation.Epoch || anchor.EventsSHA256 != observation.EventsSHA256 || anchor.DataSHA256 != observation.DataSHA256 {
		return ErrSQLiteRestoreVersionsInvalid
	}
	for _, digest := range []string{anchor.GenerationSHA256, anchor.WriterIdentitySHA256, anchor.EventsSHA256, anchor.DataSHA256} {
		if !sqliteRestoreContentValidDigest(digest) {
			return ErrSQLiteRestoreVersionsInvalid
		}
	}
	if anchor.IssuedAt.Location() != time.UTC || anchor.ExpiresAt.Location() != time.UTC || anchor.IssuedAt.IsZero() || anchor.ExpiresAt.IsZero() || anchor.IssuedAt.After(now) || !anchor.ExpiresAt.After(now) || !anchor.ExpiresAt.After(anchor.IssuedAt) || anchor.ExpiresAt.Sub(anchor.IssuedAt) > 5*time.Minute {
		return ErrSQLiteRestoreVersionsInvalid
	}
	return nil
}

func (s *SQLiteStore) readSQLiteRestoreStartupSnapshot(ctx context.Context, pin SQLiteRestoreVersionPin) (*SQLiteRestoreVersionObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	defer tx.Rollback()
	if err := sqliteRestoreStartupProfile(ctx, tx); err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	out, err := s.restoreVersions.read(ctx, tx)
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if out.Origin != pin.Origin || out.NamespaceSHA256 != pin.NamespaceSHA256 || out.SchemaSHA256 != pin.SchemaSHA256 || out.Epoch < pin.MinimumEpoch || sqliteRestoreVersionHash(out.Events[:pin.MinimumEpoch]) != pin.PrefixEventsSHA256 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := tx.Commit(); err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if err := s.restoreVersions.checkFile(); err != nil {
		return nil, err
	}
	return out, nil
}

func sqliteRestoreStartupProfile(ctx context.Context, tx *sql.Tx) error {
	var mode string
	if err := tx.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		return ErrSQLiteRestoreVersionsInvalid
	}
	for _, setting := range []struct {
		name  string
		value int
	}{{"synchronous", 2}, {"trusted_schema", 1}, {"recursive_triggers", 0}} {
		var value int
		if err := tx.QueryRowContext(ctx, "PRAGMA "+setting.name).Scan(&value); err != nil || value != setting.value {
			return ErrSQLiteRestoreVersionsInvalid
		}
	}
	var digest string
	if err := tx.QueryRowContext(ctx, `SELECT `+sqliteRestoreContentFunction+`('startup-vector-v2')`).Scan(&digest); err != nil || digest != sqliteRestoreStartupContentVector() {
		return ErrSQLiteRestoreVersionsInvalid
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name GLOB 'restore_row_*'`).Scan(&count); err != nil || count != 12 {
		return ErrSQLiteRestoreVersionsInvalid
	}
	for _, table := range sqliteRestoreVersionTables {
		columns, err := sqliteRestoreVersionColumns(ctx, tx, table.name)
		if err != nil {
			return err
		}
		for _, operation := range []string{"insert", "update", "delete"} {
			var ddl string
			err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=? AND tbl_name=?`, "restore_row_"+table.name+"_"+operation, table.name).Scan(&ddl)
			expected := sqliteRestoreVersionTrigger(table, columns, operation)
			if err != nil || strings.TrimSuffix(ddl, ";") != strings.TrimSuffix(expected, ";") {
				return ErrSQLiteRestoreVersionsInvalid
			}
		}
	}
	return nil
}

// Keep the vector independent of the registered SQL callback so startup checks
// the actual installed implementation, not just whether a function exists.
func sqliteRestoreStartupContentVector() string {
	return "6f53cf1fb12f32b245ea0dd3222cf2fe64f3c43f5ff71f7adafb7d1cf251e90e"
}

// The read-only reopened reader always uses its originally supplied pin. Startup
// validation cannot replace that pin or turn this reader into a writer.
func (r *SQLiteRestoreVersionReader) ReadSQLiteRestoreStartup(ctx context.Context, authorityIdentitySHA256 string, anchorReader SQLiteRestoreStartupAnchorReader) (*SQLiteRestoreStartupReport, error) {
	return r.store.ReadSQLiteRestoreStartup(ctx, SQLiteRestoreStartupRequest{Pin: r.pin, AuthorityIdentitySHA256: authorityIdentitySHA256, AnchorReader: anchorReader})
}
