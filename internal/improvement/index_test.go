package improvement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuxujun/ai-agent/internal/harness"
)

func indexLesson(team string, n int) Lesson {
	return Lesson{Team: team, HarnessBefore: "base", HarnessAfter: fmt.Sprint(n), Edits: []harness.Edit{{Target: "teams." + team + ".planner.model", Op: "set", After: fmt.Sprint(n)}}, ObservedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Second)}
}
func publishIndexLesson(ctx context.Context, s *Store, l Lesson) error {
	if err := s.Validate(ctx, Validation{LessonID: l.ID, ReportSHA256: strings.Repeat("a", 64), DatasetSHA256: strings.Repeat("b", 64), Cases: 2, MaxLatencyRatio: 1.2, MaxCostRatio: 1.2, MaxTaskCostUSD: .1, MaxLLMCalls: 20}); err != nil {
		return err
	}
	return s.Publish(ctx, l.ID, "index test")
}
func recordIndexLesson(t *testing.T, s *Store, l Lesson, published bool) Lesson {
	t.Helper()
	l, err := s.Record(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		if err := publishIndexLesson(t.Context(), s, l); err != nil {
			t.Fatal(err)
		}
	}
	return l
}
func lessonIDs(ls []Lesson) []string {
	var ids []string
	for _, l := range ls {
		ids = append(ids, l.ID)
	}
	return ids
}
func slowLessonList(t *testing.T, s *Store, team string, limit int, published bool) []Lesson {
	t.Helper()
	entries, err := os.ReadDir(s.lessonsDir())
	if err != nil {
		t.Fatal(err)
	}
	var result []Lesson
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		var l Lesson
		var ok bool
		if published {
			l, ok, err = s.GetPublished(t.Context(), id)
		} else {
			l, ok, err = s.Get(t.Context(), id)
		}
		if err != nil {
			t.Fatal(err)
		}
		if ok && (team == "" || l.Team == team) {
			result = append(result, l)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ObservedAt.Equal(result[j].ObservedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].ObservedAt.After(result[j].ObservedAt)
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result
}

func TestLessonIndexMatchesFreshReadsAndInvalidates(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	for i := 0; i < 40; i++ {
		l := recordIndexLesson(t, s, indexLesson(fmt.Sprintf("team-%d", i%4), i), i%3 == 0)
		if i%7 == 0 {
			if err := s.Retract(t.Context(), l.ID, "test"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, team := range []string{"", "team-0", "team-1", "missing"} {
		for _, limit := range []int{-1, 0, 1, 8} {
			for _, published := range []bool{false, true} {
				var got []Lesson
				var err error
				reader := NewStore(filepath.Join(root, "."))
				if published {
					got, err = reader.ListPublished(t.Context(), team, limit)
				} else {
					got, err = reader.List(t.Context(), team, limit)
				}
				if err != nil || !reflect.DeepEqual(lessonIDs(got), lessonIDs(slowLessonList(t, s, team, limit, published))) {
					t.Fatalf("team=%s limit=%d published=%v: %v", team, limit, published, err)
				}
			}
		}
	}
	old := recordIndexLesson(t, s, indexLesson("team-0", 100), true)
	latest := recordIndexLesson(t, NewStore(root), indexLesson("team-0", 101), false)
	got, err := s.ListPublished(t.Context(), "team-0", 1)
	if err != nil || got[0].ID != old.ID {
		t.Fatalf("candidate auto-published: %v", err)
	}
	if err := publishIndexLesson(t.Context(), NewStore(root), latest); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListPublished(t.Context(), "team-0", 1)
	if err != nil || got[0].ID != latest.ID {
		t.Fatalf("publish did not invalidate: %v", err)
	}
	old.ObservedAt = latest.ObservedAt.Add(time.Hour)
	if _, err := NewStore(root).Record(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListPublished(t.Context(), "team-0", 1)
	if err != nil || got[0].ID != old.ID {
		t.Fatalf("same-ID record did not resort: %v", err)
	}
	got[0].Edits[0].After = "mutated return"
	again, err := s.ListPublished(t.Context(), "team-0", 1)
	if err != nil || again[0].Edits[0].After == "mutated return" {
		t.Fatal("caller mutated cache")
	}
	if err := NewStore(root).Retract(t.Context(), old.ID, "withdraw"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListPublished(t.Context(), "team-0", 1)
	if err != nil || got[0].ID != latest.ID {
		t.Fatalf("retracted result served: %v", err)
	}
}

func TestLessonIndexLiveChecksBypassStaleMetadata(t *testing.T) {
	s := NewStore(t.TempDir())
	older := recordIndexLesson(t, s, indexLesson("team", 1), true)
	latest := recordIndexLesson(t, s, indexLesson("team", 2), true)
	if _, err := s.ListPublished(t.Context(), "team", 1); err != nil {
		t.Fatal(err)
	}
	// In-place writes do not change directory mtimes or the managed generation.
	path := filepath.Join(s.root, "publications", latest.ID+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"lesson_id":"wrong"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListPublished(t.Context(), "team", 1); err == nil {
		t.Fatal("cached publication bypassed validation")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	lessonPath := filepath.Join(s.lessonsDir(), latest.ID+".json")
	original, err = os.ReadFile(lessonPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := latest
	changed.Edits = append([]harness.Edit(nil), latest.Edits...)
	changed.Edits[0].After = "tampered"
	data, _ := json.Marshal(changed)
	if err := os.WriteFile(lessonPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListPublished(t.Context(), "team", 1); err == nil {
		t.Fatal("cached index bypassed lesson digest")
	}
	if err := os.WriteFile(lessonPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.retractionsDir(), 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(s.retractionsDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.retractionsDir(), latest.ID+".json"), []byte("withdrawn"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.retractionsDir(), dir.ModTime(), dir.ModTime()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != older.ID {
		t.Fatalf("live retraction ignored: %v", err)
	}
}

func TestLessonIndexTTLRootIdentityAndBound(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	older := recordIndexLesson(t, s, indexLesson("team", 1), false)
	newest := recordIndexLesson(t, s, indexLesson("team", 2), false)
	if _, err := s.List(t.Context(), "team", 1); err != nil {
		t.Fatal(err)
	}
	older.ObservedAt = newest.ObservedAt.Add(time.Hour)
	data, _ := json.Marshal(older)
	if err := os.WriteFile(filepath.Join(s.lessonsDir(), older.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	canonical, err := s.indexRoot()
	if err != nil {
		t.Fatal(err)
	}
	cache := sharedLessonIndex(canonical)
	<-cache.gate
	cache.builtAt = time.Now().Add(-lessonIndexMaxAge - time.Second)
	cache.gate <- struct{}{}
	got, err := s.List(t.Context(), "team", 1)
	if err != nil || got[0].ID != older.ID {
		t.Fatalf("TTL did not catch unmanaged metadata edit: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	aliasRoot, err := NewStore(alias).indexRoot()
	if err != nil || sharedLessonIndex(aliasRoot) != cache {
		t.Fatal("path alias did not share index")
	}
	evictedRoot := filepath.Join(t.TempDir(), "evicted")
	first := sharedLessonIndex(evictedRoot)
	for range lessonIndexRoots + 1 {
		sharedLessonIndex(t.TempDir())
	}
	lessonIndexes.Lock()
	count := len(lessonIndexes.roots)
	lessonIndexes.Unlock()
	if count > lessonIndexRoots {
		t.Fatal("unbounded root cache")
	}
	if sharedLessonIndex(evictedRoot) == first {
		t.Fatal("oldest root was not evicted")
	}
}

func TestLessonIndexCancellationAndInvalidGeneration(t *testing.T) {
	s := NewStore(t.TempDir())
	recordIndexLesson(t, s, indexLesson("team", 1), true)
	if _, err := s.List(t.Context(), "team", 1); err != nil {
		t.Fatal(err)
	}
	root, err := s.indexRoot()
	if err != nil {
		t.Fatal(err)
	}
	c := sharedLessonIndex(root)
	<-c.gate
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := s.List(ctx, "team", 1); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled lookup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache wait ignored cancellation")
	}
	c.gate <- struct{}{}
	if err := os.WriteFile(filepath.Join(s.root, lessonGenerationFile), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListPublished(t.Context(), "team", 1); err == nil {
		t.Fatal("invalid generation reused old index")
	}
}

func TestLessonIndexConcurrentWritersAndReaders(t *testing.T) {
	root := t.TempDir()
	reader := NewStore(root)
	var wg sync.WaitGroup
	for writer := range 3 {
		wg.Go(func() {
			s := NewStore(root)
			for i := range 8 {
				l, err := s.Record(t.Context(), indexLesson("team", writer*100+i))
				if err != nil {
					t.Error(err)
					return
				}
				if err := publishIndexLesson(t.Context(), s, l); err != nil {
					t.Error(err)
					return
				}
				if err := s.Retract(t.Context(), l.ID, "concurrent withdrawal"); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 3 {
		wg.Go(func() {
			for range 30 {
				if _, err := reader.ListPublished(t.Context(), "team", 1); err != nil && !errors.Is(err, errLessonIndexChanged) {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	got, err := reader.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 0 {
		t.Fatalf("withdrawn result after writers complete: %v", err)
	}
}

func TestLessonIndexProcessHelper(t *testing.T) {
	root := os.Getenv("AI_AGENT_LESSON_INDEX_HELPER_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	data, err := os.ReadFile(filepath.Join(root, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Action string
		Lesson Lesson
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	s := NewStore(root)
	if request.Action == "retract" {
		if err := s.Retract(t.Context(), request.Lesson.ID, "subprocess"); err != nil {
			t.Fatal(err)
		}
		return
	}
	l, err := s.Record(t.Context(), request.Lesson)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishIndexLesson(t.Context(), s, l); err != nil {
		t.Fatal(err)
	}
}

func TestLessonIndexCrossProcessGeneration(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	first := recordIndexLesson(t, s, indexLesson("team", 1), true)
	if err := os.MkdirAll(s.retractionsDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListPublished(t.Context(), "team", 1); err != nil {
		t.Fatal(err)
	}
	run := func(action string, l Lesson) {
		t.Helper()
		dirs := []string{s.lessonsDir(), filepath.Join(root, "publications"), s.retractionsDir()}
		times := make([]time.Time, len(dirs))
		for i, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			times[i] = info.ModTime()
		}
		data, _ := json.Marshal(struct {
			Action string
			Lesson Lesson
		}{action, l})
		if err := os.WriteFile(filepath.Join(root, "request.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLessonIndexProcessHelper$")
		cmd.Env = append(os.Environ(), "AI_AGENT_LESSON_INDEX_HELPER_ROOT="+root)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subprocess: %v %s", err, output)
		}
		// Defeat timestamp-only invalidation: the generation content must carry it.
		for i, dir := range dirs {
			if err := os.Chtimes(dir, times[i], times[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	latest := indexLesson("team", 2)
	latest.ID, _ = lessonID(latest)
	run("publish", latest)
	got, err := s.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != latest.ID {
		t.Fatalf("child publish not visible: %v", err)
	}
	run("retract", latest)
	got, err = s.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("child withdrawal not visible: %v", err)
	}
}

func TestLessonIndexRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	s := NewStore(root)
	recordIndexLesson(t, s, indexLesson("team", 100), true)
	if _, err := s.ListPublished(t.Context(), "team", 1); err != nil {
		t.Fatal(err)
	}
	generation, err := os.ReadFile(filepath.Join(root, lessonGenerationFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(parent, "old-store")); err != nil {
		t.Fatal(err)
	}
	replacement := recordIndexLesson(t, NewStore(root), indexLesson("team", 1), true)
	// A copied generation must not make another root's cached entries authoritative.
	if err := os.WriteFile(filepath.Join(root, lessonGenerationFile), generation, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != replacement.ID {
		t.Fatalf("replaced root reused old index: %v", err)
	}
}

func TestLessonIndexCandidateListDoesNotRequirePublicationsDirectory(t *testing.T) {
	s := NewStore(t.TempDir())
	l := recordIndexLesson(t, s, indexLesson("team", 1), false)
	path := filepath.Join(s.root, "publications")
	if err := os.WriteFile(path, []byte("invalid publication directory"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != l.ID {
		t.Fatalf("ordinary listing gained publication dependency: %v", err)
	}
	if _, err := s.ListPublished(t.Context(), "team", 1); err == nil {
		t.Fatal("invalid publication directory was accepted")
	}
	if err := os.Rename(path, filepath.Join(s.root, "old-publications-file")); err != nil {
		t.Fatal(err)
	}
	if err := publishIndexLesson(t.Context(), s, l); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListPublished(t.Context(), "team", 1)
	if err != nil || len(got) != 1 || got[0].ID != l.ID {
		t.Fatalf("publication repair not visible: %v", err)
	}
}
