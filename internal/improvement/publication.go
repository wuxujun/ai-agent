package improvement

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Validation binds a reviewed joint probe report to the exact content-addressed
// lesson. These files are operator-owned evidence, not a signature/trust service.
type Validation struct {
	DatasetSHA256   string    `json:"dataset_sha256"`
	MaxCostRatio    float64   `json:"max_cost_ratio"`
	MaxTaskCostUSD  float64   `json:"max_task_cost_usd"`
	LessonID        string    `json:"lesson_id"`
	ReportSHA256    string    `json:"report_sha256"`
	Cases           int       `json:"cases"`
	MaxLatencyRatio float64   `json:"max_latency_ratio"`
	MaxLLMCalls     int       `json:"max_llm_calls"`
	ValidatedAt     time.Time `json:"validated_at"`
}

type Publication struct {
	LessonID    string     `json:"lesson_id"`
	Validation  Validation `json:"validation"`
	Reason      string     `json:"reason"`
	PublishedAt time.Time  `json:"published_at"`
}

func (s *Store) Validate(ctx context.Context, v Validation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok, err := s.Get(ctx, v.LessonID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("lesson unavailable")
	}
	if !validValidation(v) {
		return fmt.Errorf("invalid joint validation evidence")
	}
	v.ValidatedAt = time.Now().UTC()
	return s.writeState("validations", v.LessonID, v)
}

func validValidation(v Validation) bool {
	return validID(v.LessonID) && validID(v.ReportSHA256) && validID(v.DatasetSHA256) && v.MaxCostRatio > 0 && v.MaxCostRatio <= 10 && v.MaxTaskCostUSD > 0 && v.MaxTaskCostUSD <= 1000 && v.Cases > 0 && v.MaxLatencyRatio > 0 && v.MaxLatencyRatio <= 10 && v.MaxLLMCalls > 0
}

func (s *Store) Publish(ctx context.Context, id, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok, err := s.Get(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("lesson unavailable")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("publication reason is required")
	}
	var v Validation
	if err := s.readState("validations", id, &v); err != nil {
		return fmt.Errorf("joint validation required: %w", err)
	}
	if !validValidation(v) || v.LessonID != id || v.ValidatedAt.IsZero() {
		return fmt.Errorf("invalid validation binding")
	}
	return s.writeState("publications", id, Publication{LessonID: id, Validation: v, Reason: reason, PublishedAt: time.Now().UTC()})
}

func (s *Store) GetPublished(ctx context.Context, id string) (Lesson, bool, error) {
	l, ok, err := s.Get(ctx, id)
	if err != nil || !ok {
		return l, false, err
	}
	var p Publication
	if err := s.readState("publications", id, &p); os.IsNotExist(err) {
		return Lesson{}, false, nil
	} else if err != nil {
		return Lesson{}, false, err
	}
	if p.LessonID != id || !validValidation(p.Validation) || p.Validation.LessonID != id || p.Validation.ValidatedAt.IsZero() || p.PublishedAt.IsZero() || strings.TrimSpace(p.Reason) == "" {
		return Lesson{}, false, fmt.Errorf("invalid publication binding")
	}
	return l, true, nil
}

func (s *Store) ListPublished(ctx context.Context, team string, limit int) ([]Lesson, error) {
	lessons, err := s.List(ctx, team, 0)
	if err != nil {
		return nil, err
	}
	var result []Lesson
	for _, l := range lessons {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		published, ok, err := s.GetPublished(ctx, l.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, published)
		}
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, nil
}

func (s *Store) Status(ctx context.Context, id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid lesson id")
	}
	if _, ok, err := s.GetPublished(ctx, id); err != nil {
		return "", err
	} else if ok {
		return "published", nil
	}
	if retracted, err := s.isRetracted(id); err != nil {
		return "", err
	} else if retracted {
		return "retracted", nil
	}
	var v Validation
	if err := s.readState("validations", id, &v); os.IsNotExist(err) {
		return "candidate", nil
	} else if err != nil {
		return "", err
	}
	if !validValidation(v) || v.LessonID != id || v.ValidatedAt.IsZero() {
		return "", fmt.Errorf("invalid validation binding")
	}
	return "validated", nil
}

func (s *Store) readState(kind, id string, out any) error {
	if !validID(id) {
		return fmt.Errorf("invalid lesson id")
	}
	data, err := os.ReadFile(filepath.Join(s.root, kind, id+".json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
func (s *Store) writeState(kind, id string, value any) error {
	dir := filepath.Join(s.root, kind)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, id+".json"), data, 0600)
}
