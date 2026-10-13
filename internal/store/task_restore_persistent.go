package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
)

var ErrTaskRestoreTarget = errors.New("invalid isolated restore directory")

const taskRestoreDatabaseName = "isolated-restore.sqlite"

// CreatePersistentTaskRestoreSandbox allocates a fresh 0700 directory beneath
// an existing parent. It never accepts an existing database or online DSN.
// On failure the directory/file is preserved for inspection, not deleted.
func CreatePersistentTaskRestoreSandbox(ctx context.Context, parent string) (*TaskRestoreSandbox, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, "", ErrTaskRestoreTarget
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return nil, "", ErrTaskRestoreTarget
	}
	dir, err := os.MkdirTemp(parent, "task-restore-")
	if err != nil {
		return nil, "", ErrTaskRestoreTarget
	}
	path := filepath.Join(dir, taskRestoreDatabaseName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, dir, ErrTaskRestoreTarget
	}
	if err := file.Close(); err != nil {
		return nil, dir, err
	}
	db, err := openTaskRestoreFile(ctx, path, "rw")
	if err != nil {
		return nil, dir, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL"); err != nil {
		db.Close()
		return nil, dir, err
	}
	if err := initializeTaskRestoreDB(ctx, db); err != nil {
		db.Close()
		return nil, dir, err
	}
	return &TaskRestoreSandbox{db: db, persistent: true}, dir, nil
}

func openTaskRestoreFile(ctx context.Context, path, mode string) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "trusted_schema(OFF)"}}
	if mode == "ro" {
		query["_pragma"] = append(query["_pragma"], "query_only(ON)")
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenPersistentTaskRestoreSandbox reopens only the dedicated format, read-only.
// Identity/schema failures cannot run migration or mutate an online database.
func OpenPersistentTaskRestoreSandbox(ctx context.Context, directory string) (*TaskRestoreSandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, ErrTaskRestoreTarget
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil || canonical != absolute {
		return nil, ErrTaskRestoreTarget
	}
	dirInfo, err := os.Lstat(absolute)
	if err != nil || !dirInfo.IsDir() || (runtime.GOOS != "windows" && dirInfo.Mode().Perm()&0077 != 0) {
		return nil, ErrTaskRestoreTarget
	}
	path := filepath.Join(absolute, taskRestoreDatabaseName)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || (runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0) {
		return nil, ErrTaskRestoreTarget
	}
	db, err := openTaskRestoreFile(ctx, path, "ro")
	if err != nil {
		return nil, ErrTaskRestoreTarget
	}
	fail := func() (*TaskRestoreSandbox, error) { db.Close(); return nil, ErrTaskRestoreTarget }
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		return fail()
	}
	var applicationID, version int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil || applicationID != taskRestoreApplicationID {
		return fail()
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != taskRestoreSchemaVersion {
		return fail()
	}
	rows, err := db.QueryContext(ctx, "SELECT type,name FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return fail()
	}
	seen := map[string]bool{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil || kind != "table" || (name != "restore_runs" && name != "restore_parts") {
			rows.Close()
			return fail()
		}
		seen[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(seen) != 2 {
		return fail()
	}
	// Check every page before exposing a handle, including pages not read by
	// identity/schema queries. A damaged sandbox must never report success.
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fail()
	}
	// Verify column contracts before exposing a reopened handle. No writes occur.
	for _, query := range []string{"SELECT task_id,tenant_id,expected,payload_hash,artifact FROM restore_runs LIMIT 0", "SELECT task_id,kind,position,body FROM restore_parts LIMIT 0"} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fail()
		}
		rows.Close()
	}
	return &TaskRestoreSandbox{db: db, persistent: true, readOnly: true}, nil
}

// VerifyPersistentTaskRestore imports, closes the writable handle, then opens
// the dedicated database read-only and verifies its actual persisted records.
// The returned directory is preserved on success and on post-creation failure.
func VerifyPersistentTaskRestore(ctx context.Context, data []byte, tenant, parent string, budget int) (*TaskRestoreReport, string, error) {
	_, snapshot, _, err := decodeJointRestore(ctx, data, tenant, budget)
	if err != nil {
		return nil, "", err
	}
	sandbox, dir, err := CreatePersistentTaskRestoreSandbox(ctx, parent)
	if err != nil {
		return nil, dir, err
	}
	if err := sandbox.ImportJoint(ctx, data, tenant, budget); err != nil {
		sandbox.Close()
		return nil, dir, err
	}
	if err := sandbox.Close(); err != nil {
		return nil, dir, err
	}
	reopened, err := OpenPersistentTaskRestoreSandbox(ctx, dir)
	if err != nil {
		return nil, dir, err
	}
	defer reopened.Close()
	report, err := reopened.Verify(ctx, snapshot.Task.ID, tenant)
	return report, dir, err
}
