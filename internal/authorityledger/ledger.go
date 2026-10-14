// Package authorityledger implements a dedicated SQLite protocol fixture. It
// is not wired into a runtime Store or an admission reader and grants no trust,
// isolation, execution, restoration, retention or deletion permissions.
package authorityledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"
)

const (
	Coverage       = "ledger-fixture-v1"
	SnapshotSchema = "authority-ledger-fixture-snapshot-v1"
	MaxEvents      = 2048
	MaxResources   = 1024
	maxBodyBytes   = 4096
)

var (
	ErrInvalid     = errors.New("invalid authority ledger fixture")
	ErrConflict    = errors.New("authority ledger conflict")
	ErrBudget      = errors.New("authority ledger budget exceeded")
	ErrReadOnly    = errors.New("authority ledger is read-only")
	ErrRollback    = errors.New("authority ledger is behind independent floor")
	ErrUnconfirmed = errors.New("authority ledger is not confirmed by independent floor")
)

type Identity struct {
	Origin          string `json:"origin"`
	NamespaceSHA256 string `json:"namespace_sha256"`
	Coverage        string `json:"coverage"`
}
type Mutation struct {
	EventID             string `json:"event_id"`
	TenantID            string `json:"tenant_id"`
	Kind                string `json:"kind"`
	ResourceID          string `json:"resource_id"`
	Operation           string `json:"operation"`
	ExpectedEpoch       int64  `json:"expected_epoch,string"`
	ExpectedIncarnation string `json:"expected_incarnation"`
	ContentSHA256       string `json:"content_sha256"`
}
type Resource struct {
	TenantID      string `json:"tenant_id"`
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Epoch         int64  `json:"epoch,string"`
	Incarnation   string `json:"incarnation"`
	Tombstone     bool   `json:"tombstone"`
	ContentSHA256 string `json:"content_sha256"`
}
type Event struct {
	Epoch          int64    `json:"epoch,string"`
	Mutation       Mutation `json:"mutation"`
	Resource       Resource `json:"resource"`
	PreviousSHA256 string   `json:"previous_sha256"`
	SHA256         string   `json:"sha256"`
}
type Snapshot struct {
	Schema     string     `json:"schema"`
	Identity   Identity   `json:"identity"`
	Epoch      int64      `json:"epoch,string"`
	HeadSHA256 string     `json:"head_sha256"`
	Resources  []Resource `json:"resources"`
	Events     []Event    `json:"events"`
}
type Receipt struct {
	Event    Event
	Replayed bool
}
type Ledger struct {
	file     *fixtureFile
	identity Identity
	readOnly bool
}

func digest(data []byte) string    { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func typedDigest(value any) string { data, _ := json.Marshal(value); return digest(data) }
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	data, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(data) == s
}
func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func validIdentity(i Identity) bool {
	return validDigest(i.Origin) && validDigest(i.NamespaceSHA256) && i.Coverage == Coverage
}
func resourceKey(tenant, kind, id string) string {
	return typedDigest(struct{ Tenant, Kind, ID string }{tenant, kind, id})
}
func genesis(i Identity) string {
	return digest([]byte("ai-agent/authority-ledger/genesis/v1\n" + typedDigest(i)))
}
func eventDigest(e Event) string {
	e.SHA256 = ""
	return digest([]byte("ai-agent/authority-ledger/event/v1\n" + typedDigest(e)))
}
func validateMutation(m Mutation) bool {
	if !validDigest(m.EventID) || !validID(m.TenantID) || !validID(m.ResourceID) || (m.Kind != "task" && m.Kind != "session") || m.ExpectedEpoch < 0 || (m.ExpectedEpoch == 0 && m.ExpectedIncarnation != "") || (m.ExpectedEpoch > 0 && !validDigest(m.ExpectedIncarnation)) {
		return false
	}
	switch m.Operation {
	case "create", "update", "invalidate":
		return validDigest(m.ContentSHA256)
	case "tombstone":
		return m.ContentSHA256 == ""
	default:
		return false
	}
}
func transition(i Identity, global int64, previous Resource, exists bool, m Mutation) (Resource, error) {
	if !validateMutation(m) || global <= 0 {
		return Resource{}, ErrInvalid
	}
	if (!exists && m.ExpectedEpoch != 0) || (exists && (m.ExpectedEpoch != previous.Epoch || m.ExpectedIncarnation != previous.Incarnation)) {
		return Resource{}, ErrConflict
	}
	if exists && previous.Epoch == math.MaxInt64 {
		return Resource{}, ErrBudget
	}
	r := Resource{TenantID: m.TenantID, Kind: m.Kind, ID: m.ResourceID, Epoch: m.ExpectedEpoch + 1, Incarnation: previous.Incarnation, ContentSHA256: m.ContentSHA256}
	switch m.Operation {
	case "create":
		if exists && !previous.Tombstone {
			return Resource{}, ErrConflict
		}
		r.Incarnation = typedDigest(struct {
			Origin, EventID string
			Epoch           int64
		}{i.Origin, m.EventID, global})
	case "update":
		if !exists || previous.Tombstone || previous.ContentSHA256 == m.ContentSHA256 {
			return Resource{}, ErrConflict
		}
	case "invalidate":
		if !exists || previous.Tombstone || previous.ContentSHA256 != m.ContentSHA256 {
			return Resource{}, ErrConflict
		}
	case "tombstone":
		if !exists || previous.Tombstone {
			return Resource{}, ErrConflict
		}
		r.Tombstone = true
	}
	return r, nil
}
func sanitize(err error) error {
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrInvalid, ErrConflict, ErrBudget, ErrReadOnly, ErrRollback, ErrUnconfirmed} {
		if errors.Is(err, known) {
			return known
		}
	}
	if err != nil {
		return ErrInvalid
	}
	return nil
}

// Apply models one fixture resource mutation and its epoch/event in the same
// transaction. It never calls a production writer or executes a callback.
// Exact event replay returns the original receipt without reapplying a mutation.
func (l *Ledger) Apply(ctx context.Context, m Mutation) (receipt *Receipt, err error) {
	defer func() { err = sanitize(err) }()
	if !validateMutation(m) {
		return nil, ErrInvalid
	}
	if l.readOnly {
		return nil, ErrReadOnly
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = l.file.lock(ctx); err != nil {
		return nil, err
	}
	defer l.file.unlock()
	if err = l.file.check(ctx, true); err != nil {
		return nil, err
	}
	tx, err := l.file.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Acquire the write lock before reading heads, including across connections.
	if _, err = tx.ExecContext(ctx, "UPDATE metadata SET epoch=epoch WHERE singleton=1"); err != nil {
		return nil, err
	}
	receipt, err = l.applyTx(ctx, tx, m)
	if err != nil {
		return nil, err
	}
	if err = l.file.check(ctx, false); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}
func (l *Ledger) applyTx(ctx context.Context, tx *sql.Tx, m Mutation) (*Receipt, error) {
	snapshot, events, err := l.snapshotTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.Mutation.EventID == m.EventID {
			if e.Mutation != m {
				return nil, ErrConflict
			}
			return &Receipt{Event: e, Replayed: true}, nil
		}
	}
	if len(events) >= MaxEvents || snapshot.Epoch == math.MaxInt64 {
		return nil, ErrBudget
	}
	key := resourceKey(m.TenantID, m.Kind, m.ResourceID)
	var previous Resource
	exists := false
	for _, r := range snapshot.Resources {
		if resourceKey(r.TenantID, r.Kind, r.ID) == key {
			previous, exists = r, true
			break
		}
	}
	if !exists && len(snapshot.Resources) >= MaxResources {
		return nil, ErrBudget
	}
	next, err := transition(l.identity, snapshot.Epoch+1, previous, exists, m)
	if err != nil {
		return nil, err
	}
	event := Event{Epoch: snapshot.Epoch + 1, Mutation: m, Resource: next, PreviousSHA256: snapshot.HeadSHA256}
	event.SHA256 = eventDigest(event)
	body, _ := json.Marshal(next)
	eventBody, _ := json.Marshal(event)
	if len(body) > maxBodyBytes || len(eventBody) > maxBodyBytes {
		return nil, ErrBudget
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO resources(scope_key,body) VALUES(?,?) ON CONFLICT(scope_key) DO UPDATE SET body=excluded.body", key, body); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO events(epoch,event_id,body) VALUES(?,?,?)", event.Epoch, m.EventID, eventBody); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE metadata SET epoch=?,head_sha256=? WHERE singleton=1 AND epoch=? AND head_sha256=?", event.Epoch, event.SHA256, snapshot.Epoch, snapshot.HeadSHA256)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return nil, ErrConflict
	}
	return &Receipt{Event: event}, nil
}

func (l *Ledger) Snapshot(ctx context.Context) (snapshot *Snapshot, err error) {
	defer func() { err = sanitize(err) }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = l.file.lock(ctx); err != nil {
		return nil, err
	}
	defer l.file.unlock()
	if err = l.file.check(ctx, true); err != nil {
		return nil, err
	}
	tx, err := l.file.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	value, _, err := l.snapshotTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = l.file.check(ctx, true); err != nil {
		return nil, err
	}
	return &value, nil
}
func (l *Ledger) snapshotTx(ctx context.Context, tx *sql.Tx) (Snapshot, []Event, error) {
	zero := Snapshot{}
	if err := validateSchemaTables(ctx, tx, "ledger"); err != nil {
		return zero, nil, err
	}
	var identity Identity
	var role string
	var epoch int64
	var head string
	if err := tx.QueryRowContext(ctx, "SELECT role,origin,namespace_sha256,coverage,epoch,head_sha256 FROM metadata WHERE singleton=1").Scan(&role, &identity.Origin, &identity.NamespaceSHA256, &identity.Coverage, &epoch, &head); err != nil {
		return zero, nil, err
	}
	if role != "ledger" || identity != l.identity || !validIdentity(identity) || epoch < 0 || epoch > MaxEvents || !validDigest(head) {
		return zero, nil, ErrInvalid
	}
	for _, table := range []struct {
		name string
		max  int
	}{{"events", MaxEvents}, {"resources", MaxResources}} {
		var count, maxSize int
		var total int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(length(body)),0),COALESCE(SUM(length(body)),0) FROM "+table.name).Scan(&count, &maxSize, &total); err != nil {
			return zero, nil, err
		}
		if count > table.max || maxSize > maxBodyBytes || total > maxDatabaseBytes {
			return zero, nil, ErrBudget
		}
		label := "scope_key"
		if table.name == "events" {
			label = "event_id"
		}
		var labelSize int
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(length(CAST("+label+" AS BLOB))),0) FROM "+table.name).Scan(&labelSize); err != nil {
			return zero, nil, err
		}
		if labelSize > 64 {
			return zero, nil, ErrBudget
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT epoch,event_id,body FROM events ORDER BY epoch")
	if err != nil {
		return zero, nil, err
	}
	events := []Event{}
	derived := map[string]Resource{}
	seen := map[string]bool{}
	chain := genesis(identity)
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			rows.Close()
			return zero, nil, err
		}
		var storedEpoch int64
		var id string
		var body []byte
		var e Event
		if err = rows.Scan(&storedEpoch, &id, &body); err != nil {
			rows.Close()
			return zero, nil, err
		}
		if !strictBody(body, &e) || e.Epoch != int64(len(events)+1) || storedEpoch != e.Epoch || id != e.Mutation.EventID || seen[id] || e.PreviousSHA256 != chain || e.SHA256 != eventDigest(e) {
			rows.Close()
			return zero, nil, ErrInvalid
		}
		key := resourceKey(e.Mutation.TenantID, e.Mutation.Kind, e.Mutation.ResourceID)
		previous, exists := derived[key]
		expected, transitionErr := transition(identity, e.Epoch, previous, exists, e.Mutation)
		if transitionErr != nil || expected != e.Resource {
			rows.Close()
			return zero, nil, ErrInvalid
		}
		derived[key] = expected
		seen[id] = true
		chain = e.SHA256
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return zero, nil, err
	}
	if epoch != int64(len(events)) || head != chain {
		return zero, nil, ErrInvalid
	}
	rows, err = tx.QueryContext(ctx, "SELECT scope_key,body FROM resources ORDER BY scope_key")
	if err != nil {
		return zero, nil, err
	}
	resources := []Resource{}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			rows.Close()
			return zero, nil, err
		}
		var key string
		var body []byte
		var resource Resource
		if err = rows.Scan(&key, &body); err != nil {
			rows.Close()
			return zero, nil, err
		}
		expected, exists := derived[key]
		if !strictBody(body, &resource) || !exists || resource != expected || key != resourceKey(resource.TenantID, resource.Kind, resource.ID) {
			rows.Close()
			return zero, nil, ErrInvalid
		}
		resources = append(resources, resource)
		delete(derived, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return zero, nil, err
	}
	if len(derived) != 0 {
		return zero, nil, ErrInvalid
	}
	return Snapshot{Schema: SnapshotSchema, Identity: identity, Epoch: epoch, HeadSHA256: head, Resources: resources, Events: events}, events, nil
}
func (l *Ledger) Identity() Identity { return l.identity }
func (l *Ledger) Close() error       { return sanitize(l.file.db.Close()) }

// SnapshotDigest is a fixture fingerprint, not an authority attestation.
func SnapshotDigest(s Snapshot) (string, error) {
	if s.Schema != SnapshotSchema || !validIdentity(s.Identity) || s.Epoch < 0 || s.Epoch > MaxEvents || !validDigest(s.HeadSHA256) || s.Resources == nil || len(s.Resources) > MaxResources || s.Events == nil || int64(len(s.Events)) != s.Epoch {
		return "", ErrInvalid
	}
	derived := map[string]Resource{}
	seen := map[string]bool{}
	chain := genesis(s.Identity)
	for index, event := range s.Events {
		if event.Epoch != int64(index+1) || !validateMutation(event.Mutation) || seen[event.Mutation.EventID] || event.PreviousSHA256 != chain {
			return "", ErrInvalid
		}
		key := resourceKey(event.Mutation.TenantID, event.Mutation.Kind, event.Mutation.ResourceID)
		previous, exists := derived[key]
		next, err := transition(s.Identity, event.Epoch, previous, exists, event.Mutation)
		if err != nil || next != event.Resource || event.SHA256 != eventDigest(event) {
			return "", ErrInvalid
		}
		derived[key] = next
		seen[event.Mutation.EventID] = true
		chain = event.SHA256
	}

	if s.HeadSHA256 != chain {
		return "", ErrInvalid
	}
	previous := ""
	for _, r := range s.Resources {
		if !validID(r.TenantID) || !validID(r.ID) || (r.Kind != "task" && r.Kind != "session") {
			return "", ErrInvalid
		}
		key := resourceKey(r.TenantID, r.Kind, r.ID)
		if key <= previous {
			return "", ErrInvalid
		}
		previous = key
		if expected, exists := derived[key]; !exists || expected != r {
			return "", ErrInvalid
		}
		delete(derived, key)
	}
	if len(derived) != 0 {
		return "", ErrInvalid
	}
	data, err := json.Marshal(s)
	if err != nil || len(data) > maxDatabaseBytes {
		return "", ErrBudget
	}
	return digest(data), nil
}
