package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// This prototype records physical core-row changes, not trusted restoration
// evidence. It deliberately has no runtime configuration or writable reopen API.
const sqliteRestoreVersionCoverage = "sqlite-core-content-versions-prototype-v2"
const sqliteRestoreVersionMaxEvents = 8192
const sqliteRestoreVersionMaxRows = 4096
const sqliteRestoreVersionMaxBytes = 8 << 20

var ErrSQLiteRestoreVersionsUnavailable = errors.New("sqlite restore versions unavailable")
var ErrSQLiteRestoreVersionsInvalid = errors.New("sqlite restore versions invalid")

type sqliteRestoreVersions struct {
	path                      string
	file                      os.FileInfo
	directory                 os.FileInfo
	origin, namespace, schema string
}

type SQLiteRestoreRowVersion struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Epoch         int64  `json:"epoch"`
	Incarnation   int64  `json:"incarnation"`
	Deleted       bool   `json:"deleted"`
	ContentSHA256 string `json:"content_sha256"`
}

type SQLiteRestoreRowEvent struct {
	Epoch        int64  `json:"epoch"`
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Operation    string `json:"operation"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
}

// SQLiteRestoreVersionObservation is an atomic, bounded local diagnostic. Epoch
// counts physical DML events, not logical saves. It never asserts complete
// session dependencies, historical contents, stopped executors or independent
// authority. DataSHA256 includes the physical rows of all four covered tables.
type SQLiteRestoreVersionObservation struct {
	Coverage        string                    `json:"coverage"`
	Status          string                    `json:"status"`
	Origin          string                    `json:"origin"`
	NamespaceSHA256 string                    `json:"namespace_sha256"`
	SchemaSHA256    string                    `json:"schema_sha256"`
	Epoch           int64                     `json:"epoch"`
	EventsSHA256    string                    `json:"events_sha256"`
	DataSHA256      string                    `json:"data_sha256"`
	Versions        []SQLiteRestoreRowVersion `json:"versions"`
	Events          []SQLiteRestoreRowEvent   `json:"events"`
	Gaps            []string                  `json:"gaps"`
}

// NewSQLiteStoreWithRestoreVersions creates a NEW dedicated private file. It
// cannot adopt an existing business database or silently backfill its history.
// Normal NewSQLiteStore remains unchanged. No production server calls this API.
func NewSQLiteStoreWithRestoreVersions(path, namespaceSHA256 string) (*SQLiteStore, error) {
	if sqliteRestoreContentRegistrationError != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if len(namespaceSHA256) != 64 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	ns, err := hex.DecodeString(namespaceSHA256)
	if err != nil || len(ns) != sha256.Size || namespaceSHA256 != strings.ToLower(namespaceSHA256) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	parent := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil || canonical != parent {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	directory, err := os.Lstat(parent)
	if err != nil || !directory.IsDir() || (runtime.GOOS != "windows" && directory.Mode().Perm()&0077 != 0) {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	// Refuse adjacent SQLite sidecars as well as an existing primary file. Failed
	// creation retains artifacts for inspection; this API does not clean them up.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = "mode=rw"
	s, err := NewSQLiteStore(uri.String())
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	// Dedicated prototype uses rollback journaling so a closed database can be
	// reopened read-only without creating WAL/shared-memory sidecars.
	if _, err := s.db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		_ = s.Close()
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close()
		}
	}()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	v := &sqliteRestoreVersions{path: path, file: info, directory: directory, origin: hex.EncodeToString(seed), namespace: namespaceSHA256}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	defer tx.Rollback()
	ddl := `CREATE TABLE restore_row_meta(singleton INTEGER PRIMARY KEY CHECK(singleton=1),origin TEXT NOT NULL,namespace_sha256 TEXT NOT NULL,coverage TEXT NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>=0 AND epoch<=8192));
CREATE TABLE restore_row_events(epoch INTEGER PRIMARY KEY,kind TEXT NOT NULL,id TEXT NOT NULL,operation TEXT NOT NULL,before_sha256 TEXT NOT NULL,after_sha256 TEXT NOT NULL);
CREATE TABLE restore_row_versions(kind TEXT NOT NULL,id TEXT NOT NULL,epoch INTEGER NOT NULL,incarnation INTEGER NOT NULL,deleted INTEGER NOT NULL CHECK(deleted IN (0,1)),content_sha256 TEXT NOT NULL,PRIMARY KEY(kind,id));`
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO restore_row_meta VALUES(1,?,?,?,0)`, v.origin, v.namespace, sqliteRestoreVersionCoverage); err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	for _, table := range sqliteRestoreVersionTables {
		cols, err := sqliteRestoreVersionColumns(ctx, tx, table.name)
		if err != nil {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		for _, operation := range []string{"insert", "update", "delete"} {
			if _, err := tx.ExecContext(ctx, sqliteRestoreVersionTrigger(table, cols, operation)); err != nil {
				return nil, ErrSQLiteRestoreVersionsInvalid
			}
		}
	}
	v.schema, err = sqliteRestoreVersionSchema(ctx, tx)
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := v.checkFile(); err != nil {
		return nil, err
	}
	s.restoreVersions = v
	ok = true
	return s, nil
}

type sqliteRestoreVersionTable struct{ name, kind, key string }

var sqliteRestoreVersionTables = []sqliteRestoreVersionTable{
	{"tasks", "task", "id"}, {"sessions", "session", "id"}, {"traces", "trace", "id"}, {"task_leases", "lease", "task_id"},
}

// Identifiers come solely from this fixed schema. Reject unknown identifier
// shapes before interpolating schema-derived columns into SQL.
func sqliteRestoreVersionColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return nil, err
		}
		if name == "" || len(name) > 128 {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		for _, c := range name {
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return nil, ErrSQLiteRestoreVersionsInvalid
			}
		}
		columns = append(columns, name)
	}
	if len(columns) == 0 || len(columns) > 128 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	return columns, rows.Err()
}

func sqliteRestoreVersionTrigger(table sqliteRestoreVersionTable, cols []string, operation string) string {
	alias, action := "NEW", "update"
	if operation == "delete" {
		alias, action = "OLD", "delete"
	} else if operation == "insert" {
		action = "create"
	}
	key := `CAST(` + alias + `."` + table.key + `" AS TEXT)`
	when := ""
	if operation == "update" {
		differences := make([]string, len(cols))
		for i, col := range cols {
			differences[i] = `OLD."` + col + `" IS NOT NEW."` + col + `"`
		}
		when = " WHEN " + strings.Join(differences, " OR ")
	}
	body := `SELECT CASE WHEN (SELECT count(*) FROM restore_row_meta)!=1 THEN RAISE(ABORT,'restore row metadata missing') END;
SELECT CASE WHEN length(` + key + `)=0 OR length(CAST(` + key + ` AS BLOB))>128 THEN RAISE(ABORT,'restore row id invalid') END;`
	if operation == "update" {
		body += `SELECT CASE WHEN OLD."` + table.key + `" IS NOT NEW."` + table.key + `" THEN RAISE(ABORT,'restore row identity immutable') END;`
	}
	before, after := "''", sqliteRestoreContentExpression(table, cols, "NEW")
	if operation != "insert" {
		before = sqliteRestoreContentExpression(table, cols, "OLD")
	}
	if operation == "delete" {
		after = "''"
	}
	body += `SELECT CASE WHEN (` + sqliteRestoreContentSize(cols, alias) + `)>8388608 THEN RAISE(ABORT,'restore row content oversized') END;`
	predicate := `kind='` + table.kind + `' AND id=` + key
	if operation == "insert" {
		body += `SELECT CASE WHEN EXISTS(SELECT 1 FROM restore_row_versions WHERE ` + predicate + ` AND deleted=0) THEN RAISE(ABORT,'restore row create conflict') END;`
	} else {
		body += `SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM restore_row_versions WHERE ` + predicate + ` AND deleted=0 AND content_sha256=` + before + `) THEN RAISE(ABORT,'restore row predecessor missing') END;`
	}
	body += `UPDATE restore_row_meta SET epoch=epoch+1 WHERE singleton=1;
INSERT INTO restore_row_events SELECT epoch,'` + table.kind + `',` + key + `,'` + action + `',` + before + `,` + after + ` FROM restore_row_meta WHERE singleton=1;`
	if operation == "insert" {
		body += `SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM restore_row_versions WHERE ` + predicate + `) AND (SELECT count(*) FROM restore_row_versions)>=4096 THEN RAISE(ABORT,'restore row capacity exceeded') END; INSERT INTO restore_row_versions SELECT '` + table.kind + `',` + key + `,epoch,epoch,0,` + after + ` FROM restore_row_meta WHERE singleton=1
ON CONFLICT(kind,id) DO UPDATE SET epoch=excluded.epoch,incarnation=excluded.incarnation,deleted=0,content_sha256=excluded.content_sha256;`
	} else {
		deleted := "0"
		if operation == "delete" {
			deleted = "1"
		}
		body += `UPDATE restore_row_versions SET epoch=(SELECT epoch FROM restore_row_meta WHERE singleton=1),deleted=` + deleted + `,content_sha256=` + after + ` WHERE ` + predicate + `;`
	}
	return `CREATE TRIGGER restore_row_` + table.name + `_` + operation + ` AFTER ` + strings.ToUpper(operation) + ` ON "` + table.name + `"` + when + ` BEGIN ` + body + ` END;`
}

func sqliteRestoreVersionHash(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func sqliteRestoreVersionSchema(ctx context.Context, tx *sql.Tx) (string, error) {
	var objectCount, schemaBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(CAST(type AS BLOB))+length(CAST(name AS BLOB))+length(CAST(tbl_name AS BLOB))+length(CAST(coalesce(sql,'') AS BLOB))),0) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`).Scan(&objectCount, &schemaBytes); err != nil {
		return "", err
	}
	if objectCount > 256 || schemaBytes < 0 || schemaBytes > 1<<20 {
		return "", ErrSQLiteRestoreVersionsInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var schema [][4]string
	bytes := 0
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			return "", err
		}
		for _, s := range row {
			bytes += len(s)
		}
		if len(schema) >= 256 || bytes > 1<<20 {
			return "", ErrSQLiteRestoreVersionsInvalid
		}
		schema = append(schema, row)
	}
	return sqliteRestoreVersionHash(schema), rows.Err()
}

func (v *sqliteRestoreVersions) checkFile() error {
	directory, err := os.Lstat(filepath.Dir(v.path))
	if err != nil || !os.SameFile(directory, v.directory) || (runtime.GOOS != "windows" && directory.Mode().Perm()&0077 != 0) {
		return ErrSQLiteRestoreVersionsInvalid
	}
	canonical, err := filepath.EvalSymlinks(filepath.Dir(v.path))
	if err != nil || canonical != filepath.Dir(v.path) {
		return ErrSQLiteRestoreVersionsInvalid
	}
	file, err := os.Lstat(v.path)
	if err != nil || !file.Mode().IsRegular() || file.Size() > 64<<20 || !os.SameFile(file, v.file) || (runtime.GOOS != "windows" && file.Mode().Perm()&0077 != 0) {
		return ErrSQLiteRestoreVersionsInvalid
	}
	return nil
}

func (s *SQLiteStore) ReadSQLiteRestoreVersions(ctx context.Context) (*SQLiteRestoreVersionObservation, error) {
	v := s.restoreVersions
	if v == nil {
		return nil, ErrSQLiteRestoreVersionsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := v.checkFile(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	defer tx.Rollback()
	observation, err := v.read(ctx, tx)
	if err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, sqliteRestoreVersionError(ctx, err)
	}
	if err := v.checkFile(); err != nil {
		return nil, err
	}
	return observation, nil
}

func sqliteRestoreVersionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSQLiteRestoreVersionsInvalid
}

func (v *sqliteRestoreVersions) read(ctx context.Context, tx *sql.Tx) (*SQLiteRestoreVersionObservation, error) {
	schema, err := sqliteRestoreVersionSchema(ctx, tx)
	if err != nil || schema != v.schema {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	out := &SQLiteRestoreVersionObservation{Status: "unverified", Versions: []SQLiteRestoreRowVersion{}, Events: []SQLiteRestoreRowEvent{}, Gaps: []string{
		"physical_rows_only", "historical_payloads_not_retained", "session_dependencies_not_bound", "execution_isolation_unverified", "independent_trust_floor_unavailable", "other_store_domains_untracked", "lease_expiry_not_materialized", "writer_permissions_unverified",
	}}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM restore_row_meta`).Scan(&count); err != nil || count != 1 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := tx.QueryRowContext(ctx, `SELECT origin,namespace_sha256,coverage,epoch FROM restore_row_meta WHERE singleton=1`).Scan(&out.Origin, &out.NamespaceSHA256, &out.Coverage, &out.Epoch); err != nil {
		return nil, err
	}
	if out.Origin != v.origin || out.NamespaceSHA256 != v.namespace || out.Coverage != sqliteRestoreVersionCoverage || out.Epoch < 0 || out.Epoch > sqliteRestoreVersionMaxEvents {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	out.SchemaSHA256 = schema
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM restore_row_events`).Scan(&count); err != nil || int64(count) != out.Epoch {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT epoch,kind,id,operation,before_sha256,after_sha256 FROM restore_row_events ORDER BY epoch LIMIT 8193`)
	if err != nil {
		return nil, err
	}
	expected := map[string]SQLiteRestoreRowVersion{}
	for rows.Next() {
		var event SQLiteRestoreRowEvent
		if err := rows.Scan(&event.Epoch, &event.Kind, &event.ID, &event.Operation, &event.BeforeSHA256, &event.AfterSHA256); err != nil {
			rows.Close()
			return nil, err
		}
		if event.Epoch != int64(len(out.Events)+1) || !sqliteRestoreVersionKind(event.Kind) || len(event.ID) == 0 || len(event.ID) > 128 {
			rows.Close()
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		key := event.Kind + "\x00" + event.ID
		version, exists := expected[key]
		if event.BeforeSHA256 != version.ContentSHA256 || (event.Operation != "delete" && !sqliteRestoreContentValidDigest(event.AfterSHA256)) || (event.Operation == "delete" && event.AfterSHA256 != "") {
			rows.Close()
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		switch event.Operation {
		case "create":
			if exists && !version.Deleted {
				rows.Close()
				return nil, ErrSQLiteRestoreVersionsInvalid
			}
			version = SQLiteRestoreRowVersion{Kind: event.Kind, ID: event.ID, Incarnation: event.Epoch}
		case "update", "delete":
			if !exists || version.Deleted {
				rows.Close()
				return nil, ErrSQLiteRestoreVersionsInvalid
			}
			version.Deleted = event.Operation == "delete"
		default:
			rows.Close()
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		version.Epoch = event.Epoch
		version.ContentSHA256 = event.AfterSHA256
		expected[key] = version
		out.Events = append(out.Events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(expected) > sqliteRestoreVersionMaxRows {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM restore_row_versions`).Scan(&count); err != nil || count != len(expected) {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	rows, err = tx.QueryContext(ctx, `SELECT kind,id,epoch,incarnation,deleted,content_sha256 FROM restore_row_versions ORDER BY kind,id LIMIT 4097`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var version SQLiteRestoreRowVersion
		var deleted int
		if err := rows.Scan(&version.Kind, &version.ID, &version.Epoch, &version.Incarnation, &deleted, &version.ContentSHA256); err != nil {
			rows.Close()
			return nil, err
		}
		version.Deleted = deleted == 1
		if deleted < 0 || deleted > 1 || version != expected[version.Kind+"\x00"+version.ID] {
			rows.Close()
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		out.Versions = append(out.Versions, version)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out.EventsSHA256 = sqliteRestoreVersionHash(out.Events)
	out.DataSHA256, err = sqliteRestoreVersionData(ctx, tx, expected)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func sqliteRestoreVersionKind(kind string) bool {
	for _, table := range sqliteRestoreVersionTables {
		if table.kind == kind {
			return true
		}
	}
	return false
}

type sqliteRestoreVersionCell struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
type sqliteRestoreVersionDataTable struct {
	Name    string                       `json:"name"`
	Columns []string                     `json:"columns"`
	Rows    [][]sqliteRestoreVersionCell `json:"rows"`
}

// Values are normalized without exposing contents in the returned diagnostic.
// SQL length preflight bounds allocation even for a single oversized text/blob.
func sqliteRestoreVersionData(ctx context.Context, tx *sql.Tx, versions map[string]SQLiteRestoreRowVersion) (string, error) {
	data := []sqliteRestoreVersionDataTable{}
	remaining := sqliteRestoreVersionMaxBytes
	live := map[string]bool{}
	for _, table := range sqliteRestoreVersionTables {
		columns, err := sqliteRestoreVersionColumns(ctx, tx, table.name)
		if err != nil {
			return "", err
		}
		quoted := make([]string, len(columns))
		sizes := make([]string, len(columns))
		for i, col := range columns {
			quoted[i] = `"` + col + `"`
			sizes[i] = `coalesce(length(CAST("` + col + `" AS BLOB)),0)`
		}
		var count, size int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(`+strings.Join(sizes, "+")+`),0) FROM "`+table.name+`"`).Scan(&count, &size); err != nil {
			return "", err
		}
		if count > sqliteRestoreVersionMaxRows || size < 0 || size > int64(remaining) {
			return "", ErrSQLiteRestoreVersionsInvalid
		}
		remaining -= int(size)
		rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(quoted, ",")+`,`+sqliteRestoreContentExpression(table, columns, "")+` FROM "`+table.name+`" ORDER BY "`+table.key+`" LIMIT 4097`)
		if err != nil {
			return "", err
		}
		entry := sqliteRestoreVersionDataTable{Name: table.name, Columns: columns, Rows: [][]sqliteRestoreVersionCell{}}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns)+1)
			var contentSHA string
			dest[len(columns)] = &contentSHA
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return "", err
			}
			cells := make([]sqliteRestoreVersionCell, len(columns))
			id := ""
			for i, value := range values {
				cell := sqliteRestoreVersionCell{}
				switch x := value.(type) {
				case nil:
					cell.Type = "null"
				case string:
					cell.Type = "text"
					cell.Value = x
				case []byte:
					cell.Type = "blob"
					cell.Value = base64.StdEncoding.EncodeToString(x)
				case int64:
					cell.Type = "integer"
					cell.Value = strconv.FormatInt(x, 10)
				case float64:
					cell.Type = "real"
					cell.Value = strconv.FormatFloat(x, 'g', -1, 64)
				case time.Time:
					cell.Type = "time"
					cell.Value = x.UTC().Format(time.RFC3339Nano)
				default:
					rows.Close()
					return "", ErrSQLiteRestoreVersionsInvalid
				}
				if columns[i] == table.key {
					id = cell.Value
				}
				cells[i] = cell
			}
			key := table.kind + "\x00" + id
			version, ok := versions[key]
			if !ok || version.Deleted || live[key] || version.ContentSHA256 != contentSHA {
				rows.Close()
				return "", ErrSQLiteRestoreVersionsInvalid
			}
			live[key] = true
			entry.Rows = append(entry.Rows, cells)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		data = append(data, entry)
	}
	for key, version := range versions {
		if !version.Deleted && !live[key] {
			return "", ErrSQLiteRestoreVersionsInvalid
		}
	}
	return sqliteRestoreVersionHash(data), nil
}

// SQLiteRestoreVersionPin must be supplied by the caller from an independently
// retained observation. This local API is not an authenticated trust provider.
type SQLiteRestoreVersionPin struct {
	Origin             string
	NamespaceSHA256    string
	SchemaSHA256       string
	MinimumEpoch       int64
	PrefixEventsSHA256 string
}

type SQLiteRestoreVersionReader struct {
	store *SQLiteStore
	pin   SQLiteRestoreVersionPin
}

func OpenSQLiteRestoreVersionReader(path string, pin SQLiteRestoreVersionPin) (*SQLiteRestoreVersionReader, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || pin.MinimumEpoch < 0 || pin.MinimumEpoch > sqliteRestoreVersionMaxEvents {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	for _, digest := range []string{pin.Origin, pin.NamespaceSHA256, pin.SchemaSHA256, pin.PrefixEventsSHA256} {
		if len(digest) != 64 {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 || digest != strings.ToLower(digest) {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
	}
	file, err := os.Lstat(path)
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	v := &sqliteRestoreVersions{path: path, file: file, directory: directory, origin: pin.Origin, namespace: pin.NamespaceSHA256, schema: pin.SchemaSHA256}
	if err := v.checkFile(); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, ErrSQLiteRestoreVersionsInvalid
		}
	}
	uri := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)", "busy_timeout(5000)"}}
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	reader := &SQLiteRestoreVersionReader{store: &SQLiteStore{db: db, restoreVersions: v}, pin: pin}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := reader.ReadSQLiteRestoreVersions(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return reader, nil
}

func (r *SQLiteRestoreVersionReader) Close() error { return r.store.Close() }
func (r *SQLiteRestoreVersionReader) ReadSQLiteRestoreVersions(ctx context.Context) (*SQLiteRestoreVersionObservation, error) {
	out, err := r.store.ReadSQLiteRestoreVersions(ctx)
	if err != nil {
		return nil, err
	}
	if out.Epoch < r.pin.MinimumEpoch || sqliteRestoreVersionHash(out.Events[:r.pin.MinimumEpoch]) != r.pin.PrefixEventsSHA256 {
		return nil, ErrSQLiteRestoreVersionsInvalid
	}
	return out, nil
}

func sqliteRestoreContentValidDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	raw, err := hex.DecodeString(digest)
	return err == nil && len(raw) == sha256.Size && strings.ToLower(digest) == digest
}
