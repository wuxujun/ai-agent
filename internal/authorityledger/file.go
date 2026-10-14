package authorityledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

const maxDatabaseBytes = 16 << 20
const ledgerApplicationID = 0x414c4331
const floorApplicationID = 0x414c4631
const schemaVersion = 1
const metadataDDL = `CREATE TABLE metadata(singleton INTEGER PRIMARY KEY CHECK(singleton=1),role TEXT NOT NULL,origin TEXT NOT NULL,namespace_sha256 TEXT NOT NULL,coverage TEXT NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>=0),head_sha256 TEXT NOT NULL);`
const ledgerDDL = `CREATE TABLE resources(scope_key TEXT PRIMARY KEY,body BLOB NOT NULL);
CREATE TABLE events(epoch INTEGER PRIMARY KEY CHECK(epoch>0),event_id TEXT NOT NULL UNIQUE,body BLOB NOT NULL);`
const floorDDL = `CREATE TABLE floor_state(singleton INTEGER PRIMARY KEY CHECK(singleton=1),source_identity BLOB NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>=-1),head_sha256 TEXT NOT NULL,snapshot_sha256 TEXT NOT NULL);`

type fixtureFile struct {
	db                      *sql.DB
	directory, path         string
	directoryInfo, fileInfo os.FileInfo
	role                    string
	gate                    chan struct{}
}

func strictBody(data []byte, target any) bool {
	if len(data) == 0 || len(data) > maxBodyBytes {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	canonical, err := json.Marshal(target)
	// Persisted bodies must be byte-for-byte canonical. This also rejects duplicate
	// keys, omitted fields, alternate integer spelling and trailing JSON.
	return err == nil && bytes.Equal(data, canonical)
}
func randomOrigin() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", ErrInvalid
	}
	return hex.EncodeToString(data), nil
}
func applicationID(role string) int {
	if role == "ledger" {
		return ledgerApplicationID
	}
	return floorApplicationID
}
func fixtureName(role string) string {
	if role == "ledger" {
		return "ledger.sqlite"
	}
	return "floor.sqlite"
}
func openFile(ctx context.Context, path, mode string) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "trusted_schema(OFF)"}}
	if mode == "ro" {
		query["_pragma"] = append(query["_pragma"], "query_only(ON)")
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, ErrInvalid
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, sanitize(err)
	}
	return db, nil
}
func (f *fixtureFile) check(ctx context.Context, noJournal bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(f.directory)
	if err != nil || canonical != f.directory {
		return ErrInvalid
	}
	directory, err := os.Lstat(f.directory)
	if err != nil || !os.SameFile(f.directoryInfo, directory) || !directory.IsDir() || (runtime.GOOS != "windows" && directory.Mode().Perm()&0077 != 0) {
		return ErrInvalid
	}
	file, err := os.Lstat(f.path)
	if err != nil || !file.Mode().IsRegular() || !os.SameFile(f.fileInfo, file) || file.Size() > maxDatabaseBytes || (runtime.GOOS != "windows" && file.Mode().Perm()&0077 != 0) {
		return ErrInvalid
	}
	if noJournal {
		for _, suffix := range []string{"-journal", "-wal", "-shm"} {
			if _, err := os.Lstat(f.path + suffix); err == nil || !os.IsNotExist(err) {
				return ErrInvalid
			}
		}
	}
	return nil
}
func createFile(ctx context.Context, parent, role, namespace string) (file *fixtureFile, identity Identity, directory string, err error) {
	if ctx.Err() != nil {
		return nil, identity, "", ctx.Err()
	}
	if !validDigest(namespace) {
		return nil, identity, "", ErrInvalid
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, identity, "", ErrInvalid
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, identity, "", ErrInvalid
	}
	directory, err = os.MkdirTemp(parent, "authority-"+role+"-")
	if err != nil {
		return nil, identity, "", ErrInvalid
	}
	path := filepath.Join(directory, fixtureName(role))
	raw, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, identity, directory, ErrInvalid
	}
	if err = raw.Close(); err != nil {
		return nil, identity, directory, ErrInvalid
	}
	origin, err := randomOrigin()
	if err != nil {
		return nil, identity, directory, err
	}
	identity = Identity{Origin: origin, NamespaceSHA256: namespace, Coverage: Coverage}
	db, err := openFile(ctx, path, "rw")
	if err != nil {
		return nil, identity, directory, err
	}
	fail := func(e error) (*fixtureFile, Identity, string, error) {
		db.Close()
		return nil, identity, directory, sanitize(e)
	}
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL"); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	ddl := ledgerDDL
	if role == "floor" {
		ddl = floorDDL
	}
	if _, err = tx.ExecContext(ctx, metadataDDL+ddl); err != nil {
		return fail(err)
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=%d", applicationID(role), schemaVersion)); err != nil {
		return fail(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO metadata VALUES(1,?,?,?,?,0,?)", role, identity.Origin, namespace, Coverage, genesis(identity)); err != nil {
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return fail(err)
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return fail(err)
	}
	return &fixtureFile{db: db, directory: directory, path: path, directoryInfo: directoryInfo, fileInfo: fileInfo, role: role, gate: make(chan struct{}, 1)}, identity, directory, nil
}
func readIdentity(ctx context.Context, db *sql.DB, role string) (Identity, error) {
	var identity Identity
	var actualRole string
	var count, size, invalidEpoch int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(length(CAST(role AS BLOB))+length(CAST(origin AS BLOB))+length(CAST(namespace_sha256 AS BLOB))+length(CAST(coverage AS BLOB))+length(CAST(head_sha256 AS BLOB))),0),COALESCE(SUM(CASE WHEN typeof(epoch)='integer' THEN 0 ELSE 1 END),0) FROM metadata`).Scan(&count, &size, &invalidEpoch); err != nil || count != 1 || size > 512 || invalidEpoch != 0 {
		return identity, ErrInvalid
	}
	if err := db.QueryRowContext(ctx, "SELECT role,origin,namespace_sha256,coverage FROM metadata WHERE singleton=1").Scan(&actualRole, &identity.Origin, &identity.NamespaceSHA256, &identity.Coverage); err != nil || actualRole != role || !validIdentity(identity) {
		return identity, ErrInvalid
	}
	return identity, nil
}

type sqlReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateSchemaTables(ctx context.Context, db sqlReader, role string) error {
	var app, version int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil || app != applicationID(role) {
		return ErrInvalid
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		return ErrInvalid
	}
	expected := map[string]bool{"metadata": false}
	if role == "ledger" {
		expected["events"] = false
		expected["resources"] = false
	} else {
		expected["floor_state"] = false
	}
	rows, err := db.QueryContext(ctx, "SELECT type,name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return ErrInvalid
	}
	for rows.Next() {
		var kind, name string
		if err = rows.Scan(&kind, &name); err != nil {
			rows.Close()
			return ErrInvalid
		}
		if _, ok := expected[name]; !ok || kind != "table" {
			rows.Close()
			return ErrInvalid
		}
		expected[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrInvalid
	}
	for _, present := range expected {
		if !present {
			return ErrInvalid
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM metadata").Scan(&count); err != nil || count != 1 {
		return ErrInvalid
	}
	return nil
}
func validateSchema(ctx context.Context, file *fixtureFile) error {
	if err := validateSchemaTables(ctx, file.db, file.role); err != nil {
		return err
	}
	var result string
	if err := file.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return ErrInvalid
	}
	return nil
}
func openReadOnly(ctx context.Context, directory, role string) (file *fixtureFile, identity Identity, err error) {
	if ctx.Err() != nil {
		return nil, identity, ctx.Err()
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, identity, ErrInvalid
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil || canonical != absolute {
		return nil, identity, ErrInvalid
	}
	path := filepath.Join(absolute, fixtureName(role))
	directoryInfo, err := os.Lstat(absolute)
	if err != nil {
		return nil, identity, ErrInvalid
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return nil, identity, ErrInvalid
	}
	file = &fixtureFile{directory: absolute, path: path, directoryInfo: directoryInfo, fileInfo: fileInfo, role: role, gate: make(chan struct{}, 1)}
	if err = file.check(ctx, true); err != nil {
		return nil, identity, err
	}
	db, err := openFile(ctx, path, "ro")
	if err != nil {
		return nil, identity, err
	}
	file.db = db
	fail := func(e error) (*fixtureFile, Identity, error) { db.Close(); return nil, identity, sanitize(e) }
	if err = file.check(ctx, true); err != nil {
		return fail(err)
	}
	if err = validateSchema(ctx, file); err != nil {
		return fail(err)
	}
	identity, err = readIdentity(ctx, db, role)
	if err != nil {
		return fail(err)
	}
	return file, identity, nil
}

// Create always allocates a new private directory and never opens an existing
// database writable. Failure preserves the directory/files for inspection.
func Create(ctx context.Context, parent, namespaceSHA256 string) (ledger *Ledger, directory string, err error) {
	file, identity, directory, err := createFile(ctx, parent, "ledger", namespaceSHA256)
	if err != nil {
		return nil, directory, err
	}
	return &Ledger{file: file, identity: identity}, directory, nil
}

// OpenReadOnly never initializes, migrates or repairs the existing fixture.
func OpenReadOnly(ctx context.Context, directory string) (*Ledger, error) {
	file, identity, err := openReadOnly(ctx, directory, "ledger")
	if err != nil {
		return nil, err
	}
	ledger := &Ledger{file: file, identity: identity, readOnly: true}
	if _, err = ledger.Snapshot(ctx); err != nil {
		file.db.Close()
		return nil, err
	}
	return ledger, nil
}

func (f *fixtureFile) lock(ctx context.Context) error {
	select {
	case f.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fixtureFile) unlock() { <-f.gate }
