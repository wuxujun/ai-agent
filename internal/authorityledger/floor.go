package authorityledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Floor is an independent protocol fixture, not an authenticated control plane.
// The caller chooses its source pin independently and passes already-validated
// snapshots. C01 does not authenticate caller-supplied snapshots or permissions.
// Real trust identities, rollback-resistant storage and transport are C04 work.
type Floor struct {
	file             *fixtureFile
	identity, source Identity
	readOnly         bool
}
type FloorState struct {
	Source         Identity `json:"source"`
	Epoch          int64    `json:"epoch,string"`
	HeadSHA256     string   `json:"head_sha256"`
	SnapshotSHA256 string   `json:"snapshot_sha256"`
}

func CreateFloor(ctx context.Context, parent string, source Identity) (floor *Floor, directory string, err error) {
	if !validIdentity(source) {
		return nil, "", ErrInvalid
	}
	file, identity, directory, err := createFile(ctx, parent, "floor", source.NamespaceSHA256)
	if err != nil {
		return nil, directory, err
	}
	data, _ := json.Marshal(source)
	if _, err = file.db.ExecContext(ctx, "INSERT INTO floor_state VALUES(1,?,-1,'','')", data); err != nil {
		file.db.Close()
		return nil, directory, sanitize(err)
	}
	return &Floor{file: file, identity: identity, source: source}, directory, nil
}
func OpenFloorReadOnly(ctx context.Context, directory string) (*Floor, error) {
	file, identity, err := openReadOnly(ctx, directory, "floor")
	if err != nil {
		return nil, err
	}
	var data []byte
	var source Identity
	var size int
	if err = file.db.QueryRowContext(ctx, "SELECT length(source_identity) FROM floor_state WHERE singleton=1").Scan(&size); err != nil || size <= 0 || size > maxBodyBytes {
		file.db.Close()
		return nil, ErrInvalid
	}
	if err = file.db.QueryRowContext(ctx, "SELECT source_identity FROM floor_state WHERE singleton=1").Scan(&data); err != nil || !strictBody(data, &source) || !validIdentity(source) {
		file.db.Close()
		return nil, ErrInvalid
	}
	floor := &Floor{file: file, identity: identity, source: source, readOnly: true}
	if _, err = floor.State(ctx); err != nil {
		file.db.Close()
		return nil, err
	}
	return floor, nil
}
func (f *Floor) load(ctx context.Context, tx *sql.Tx) (FloorState, error) {
	if err := validateSchemaTables(ctx, tx, "floor"); err != nil {
		return FloorState{}, err
	}
	var identity Identity
	var role string
	var epoch int64
	var head string
	if err := tx.QueryRowContext(ctx, "SELECT role,origin,namespace_sha256,coverage,epoch,head_sha256 FROM metadata WHERE singleton=1").Scan(&role, &identity.Origin, &identity.NamespaceSHA256, &identity.Coverage, &epoch, &head); err != nil {
		return FloorState{}, err
	}
	if role != "floor" || identity != f.identity || epoch != 0 || head != genesis(identity) {
		return FloorState{}, ErrInvalid
	}
	var count, size, digestSize, invalidEpoch int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(length(source_identity)),0),COALESCE(MAX(length(CAST(head_sha256 AS BLOB))+length(CAST(snapshot_sha256 AS BLOB))),0),COALESCE(SUM(CASE WHEN typeof(epoch)='integer' THEN 0 ELSE 1 END),0) FROM floor_state").Scan(&count, &size, &digestSize, &invalidEpoch); err != nil {
		return FloorState{}, err
	}
	if count != 1 || size > maxBodyBytes || digestSize > 128 || invalidEpoch != 0 {
		return FloorState{}, ErrInvalid
	}
	var body []byte
	var state FloorState
	if err := tx.QueryRowContext(ctx, "SELECT source_identity,epoch,head_sha256,snapshot_sha256 FROM floor_state WHERE singleton=1").Scan(&body, &state.Epoch, &state.HeadSHA256, &state.SnapshotSHA256); err != nil {
		return FloorState{}, err
	}
	if !strictBody(body, &state.Source) || state.Source != f.source || state.Epoch < -1 || state.Epoch > MaxEvents {
		return FloorState{}, ErrInvalid
	}
	if state.Epoch == -1 {
		if state.HeadSHA256 != "" || state.SnapshotSHA256 != "" {
			return FloorState{}, ErrInvalid
		}
	} else if !validDigest(state.HeadSHA256) || !validDigest(state.SnapshotSHA256) {
		return FloorState{}, ErrInvalid
	}
	return state, nil
}
func (f *Floor) State(ctx context.Context) (state *FloorState, err error) {
	defer func() { err = sanitize(err) }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = f.file.lock(ctx); err != nil {
		return nil, err
	}
	defer f.file.unlock()
	if err = f.file.check(ctx, true); err != nil {
		return nil, err
	}
	tx, err := f.file.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	value, err := f.load(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = f.file.check(ctx, true); err != nil {
		return nil, err
	}
	return &value, nil
}

// Confirm advances the independent fixture floor. Same-epoch replay is accepted
// only for the identical snapshot. It never writes to the source ledger.
func (f *Floor) Confirm(ctx context.Context, s Snapshot) (err error) {
	defer func() { err = sanitize(err) }()
	if f.readOnly {
		return ErrReadOnly
	}
	hash, err := SnapshotDigest(s)
	if err != nil {
		return err
	}
	if s.Identity != f.source {
		return ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = f.file.lock(ctx); err != nil {
		return err
	}
	defer f.file.unlock()
	if err = f.file.check(ctx, true); err != nil {
		return err
	}
	tx, err := f.file.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET epoch=epoch WHERE singleton=1"); err != nil {
		return err
	}
	state, err := f.load(ctx, tx)
	if err != nil {
		return err
	}
	if s.Epoch < state.Epoch {
		return ErrRollback
	}
	if state.Epoch > 0 && s.Events[state.Epoch-1].SHA256 != state.HeadSHA256 {
		return ErrConflict
	}
	if s.Epoch == state.Epoch {
		if s.HeadSHA256 != state.HeadSHA256 || hash != state.SnapshotSHA256 {
			return ErrConflict
		}
	} else if _, err = tx.ExecContext(ctx, "UPDATE floor_state SET epoch=?,head_sha256=?,snapshot_sha256=? WHERE singleton=1", s.Epoch, s.HeadSHA256, hash); err != nil {
		return err
	}
	if err = f.file.check(ctx, false); err != nil {
		return err
	}
	return tx.Commit()
}

// Matches proves only equality with this independently persisted fixture floor.
// A source ahead of the floor is unconfirmed, never implicitly acknowledged.
func (f *Floor) Matches(ctx context.Context, s Snapshot) error {
	hash, err := SnapshotDigest(s)
	if err != nil {
		return err
	}
	if s.Identity != f.source {
		return ErrConflict
	}
	state, err := f.State(ctx)
	if err != nil {
		return err
	}
	if s.Epoch < state.Epoch {
		return ErrRollback
	}
	if s.Epoch > state.Epoch {
		return ErrUnconfirmed
	}
	if s.HeadSHA256 != state.HeadSHA256 || hash != state.SnapshotSHA256 {
		return ErrConflict
	}
	return nil
}
func (f *Floor) Close() error { return sanitize(f.file.db.Close()) }
