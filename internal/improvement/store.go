package improvement

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a file-backed improvement harness. All methods are safe for
// concurrent use: lesson files are content-addressed (idempotent writes) and
// retractions are separate files, so readers never need locks.
type Store struct {
	root string
}

// NewStore creates a store rooted at dir. The directory is created on first write.
func NewStore(root string) *Store {
	return &Store{root: root}
}

func (s *Store) lessonsDir() string     { return filepath.Join(s.root, "lessons") }
func (s *Store) retractionsDir() string { return filepath.Join(s.root, "retractions") }

// Record validates and stores a lesson, assigning its content-addressed ID.
// Re-recording identical content is idempotent and returns the same lesson.
func (s *Store) Record(ctx context.Context, l Lesson) (Lesson, error) {
	if err := ctx.Err(); err != nil {
		return Lesson{}, err
	}
	if l.ObservedAt.IsZero() {
		l.ObservedAt = time.Now().UTC()
	}
	if err := l.validate(); err != nil {
		return Lesson{}, err
	}
	id, err := lessonID(l)
	if err != nil {
		return Lesson{}, err
	}
	l.ID = id

	if err := os.MkdirAll(s.lessonsDir(), 0o755); err != nil {
		return Lesson{}, fmt.Errorf("mkdir lessons: %w", err)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return Lesson{}, err
	}
	if err := writeAtomic(filepath.Join(s.lessonsDir(), id+".json"), data, 0o644); err != nil {
		return Lesson{}, err
	}
	return l, nil
}

// Get returns a lesson by ID, reporting whether it exists and is not retracted.
func (s *Store) Get(_ context.Context, id string) (Lesson, bool, error) {
	if !validID(id) {
		return Lesson{}, false, fmt.Errorf("invalid lesson id %q", id)
	}
	if retracted, err := s.isRetracted(id); err != nil || retracted {
		return Lesson{}, false, err
	}
	l, err := s.readLesson(id)
	if err != nil {
		return Lesson{}, false, err
	}
	return l, true, nil
}

// List returns non-retracted lessons for a team (empty team = all teams),
// newest first, capped at limit (<=0 means no cap).
func (s *Store) List(_ context.Context, team string, limit int) ([]Lesson, error) {
	entries, err := os.ReadDir(s.lessonsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lessons: %w", err)
	}
	var out []Lesson
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		retracted, err := s.isRetracted(id)
		if err != nil {
			return nil, err
		}
		if retracted {
			continue
		}
		l, err := s.readLesson(id)
		if err != nil {
			return nil, err
		}
		if team != "" && l.Team != team {
			continue
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObservedAt.After(out[j].ObservedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Retract logically withdraws a lesson. The lesson file is kept; its
// retraction record excludes it from List/Get. Retracting twice is an error.
func (s *Store) Retract(ctx context.Context, id, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(id) {
		return fmt.Errorf("invalid lesson id %q", id)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("retraction reason is required")
	}
	if _, err := s.readLesson(id); err != nil {
		return fmt.Errorf("retract: %w", err)
	}
	if retracted, err := s.isRetracted(id); err != nil {
		return err
	} else if retracted {
		return fmt.Errorf("lesson %s already retracted", shortID(id))
	}
	if err := os.MkdirAll(s.retractionsDir(), 0o755); err != nil {
		return fmt.Errorf("mkdir retractions: %w", err)
	}
	r := Retraction{LessonID: id, Reason: reason, RetractedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.retractionsDir(), id+".json"), data, 0o644)
}

func (s *Store) readLesson(id string) (Lesson, error) {
	data, err := os.ReadFile(filepath.Join(s.lessonsDir(), id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Lesson{}, fmt.Errorf("lesson %s not found", shortID(id))
		}
		return Lesson{}, err
	}
	var l Lesson
	if err := json.Unmarshal(data, &l); err != nil {
		return Lesson{}, fmt.Errorf("parse lesson %s: %w", shortID(id), err)
	}
	return l, nil
}

func (s *Store) isRetracted(id string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.retractionsDir(), id+".json"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// writeAtomic writes data to a temp file in the same directory and renames it,
// so readers never observe a partial write.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ListAssessments returns assessments for a team (empty team = all teams),
// newest first, capped at limit (<=0 means no cap).
func (s *Store) ListAssessments(_ context.Context, team string, limit int) ([]Assessment, error) {
	entries, err := os.ReadDir(s.assessmentsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read assessments: %w", err)
	}
	var out []Assessment
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.assessmentsDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var a Assessment
		if err := json.Unmarshal(data, &a); err != nil {
			return nil, fmt.Errorf("parse assessment %s: %w", e.Name(), err)
		}
		if team != "" && a.Team != team {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObservedAt.After(out[j].ObservedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordAssessment validates and stores an assessment. Re-recording identical
// content is idempotent.
func (s *Store) RecordAssessment(ctx context.Context, a Assessment) (Assessment, error) {
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}
	if a.ObservedAt.IsZero() {
		a.ObservedAt = time.Now().UTC()
	}
	if err := a.validate(); err != nil {
		return Assessment{}, err
	}
	id, err := assessmentID(a)
	if err != nil {
		return Assessment{}, err
	}
	a.ID = id
	if err := os.MkdirAll(s.assessmentsDir(), 0o755); err != nil {
		return Assessment{}, fmt.Errorf("mkdir assessments: %w", err)
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return Assessment{}, err
	}
	if err := writeAtomic(filepath.Join(s.assessmentsDir(), id+".json"), data, 0o644); err != nil {
		return Assessment{}, err
	}
	return a, nil
}

func (s *Store) assessmentsDir() string { return filepath.Join(s.root, "assessments") }
