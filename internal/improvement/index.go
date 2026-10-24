package improvement

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	lessonIndexRoots     = 32
	lessonIndexMaxAge    = 5 * time.Second
	lessonGenerationFile = ".lesson-index-generation"
)

var errLessonIndexChanged = errors.New("lesson index changed during rebuild")

type lessonIndexEntry struct {
	id, team   string
	observedAt time.Time
}
type lessonIndex struct {
	publicationErr        error
	all, published        []lessonIndexEntry
	teams, publishedTeams map[string][]lessonIndexEntry
}
type lessonIndexStamp struct {
	generation string
	root       os.FileInfo
	dirs       [3]os.FileInfo
}

func (a lessonIndexStamp) equal(b lessonIndexStamp) bool {
	same := func(a, b os.FileInfo, mtime bool) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return os.SameFile(a, b) && a.Mode() == b.Mode() && (!mtime || a.ModTime().Equal(b.ModTime()))
	}
	if a.generation != b.generation || !same(a.root, b.root, false) {
		return false
	}
	for i := range a.dirs {
		if !same(a.dirs[i], b.dirs[i], true) {
			return false
		}
	}
	return true
}

type cachedLessonIndex struct {
	gate    chan struct{}
	index   *lessonIndex
	stamp   lessonIndexStamp
	builtAt time.Time
}
type lessonIndexRoot struct {
	root  string
	cache *cachedLessonIndex
}

var lessonIndexes = struct {
	sync.Mutex
	roots map[string]*list.Element
	lru   *list.List
}{roots: map[string]*list.Element{}, lru: list.New()}

func sharedLessonIndex(root string) *cachedLessonIndex {
	lessonIndexes.Lock()
	defer lessonIndexes.Unlock()
	if entry := lessonIndexes.roots[root]; entry != nil {
		lessonIndexes.lru.MoveToFront(entry)
		return entry.Value.(lessonIndexRoot).cache
	}
	c := &cachedLessonIndex{gate: make(chan struct{}, 1)}
	c.gate <- struct{}{}
	lessonIndexes.roots[root] = lessonIndexes.lru.PushFront(lessonIndexRoot{root: root, cache: c})
	if len(lessonIndexes.roots) > lessonIndexRoots {
		last := lessonIndexes.lru.Back()
		delete(lessonIndexes.roots, last.Value.(lessonIndexRoot).root)
		lessonIndexes.lru.Remove(last)
	}
	return c
}

func (s *Store) indexRoot() (string, error) {
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(root)
	if os.IsNotExist(err) {
		return root, nil
	}
	return real, err
}

// Only metadata is cached. Every returned lesson/publication/retraction is read
// live; a stale index must never authorize a withdrawn or modified lesson.
func (s *Store) listIndexed(ctx context.Context, team string, limit int, published bool) ([]Lesson, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := s.indexRoot()
	if err != nil {
		return nil, err
	}
	bound := &Store{root: root}
	index, err := bound.loadLessonIndex(ctx, sharedLessonIndex(root))
	if err != nil {
		return nil, err
	}
	if published && index.publicationErr != nil {
		return nil, index.publicationErr
	}
	entries := index.all
	if team != "" {
		entries = index.teams[team]
	}
	if published {
		entries = index.published
		if team != "" {
			entries = index.publishedTeams[team]
		}
	}
	var result []Lesson
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var l Lesson
		var ok bool
		if published {
			l, ok, err = bound.GetPublished(ctx, entry.id)
		} else {
			l, ok, err = bound.Get(ctx, entry.id)
		}
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, l)
		}
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) loadLessonIndex(ctx context.Context, c *cachedLessonIndex) (*lessonIndex, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	for range 3 {
		before, err := s.lessonIndexStamp(ctx)
		if err != nil {
			return nil, err
		}
		if c.index != nil && time.Since(c.builtAt) < lessonIndexMaxAge && c.stamp.equal(before) {
			return c.index, nil
		}
		index, buildErr := s.buildLessonIndex(ctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		after, err := s.lessonIndexStamp(ctx)
		if err != nil {
			return nil, err
		}
		if !before.equal(after) {
			continue
		}
		if buildErr != nil {
			return nil, buildErr
		}
		c.index = index
		c.stamp = after
		c.builtAt = time.Now()
		return index, nil
	}
	return nil, errLessonIndexChanged
}

func (s *Store) lessonIndexStamp(ctx context.Context) (lessonIndexStamp, error) {
	var stamp lessonIndexStamp
	if err := ctx.Err(); err != nil {
		return stamp, err
	}
	var err error
	stamp.root, err = os.Stat(s.root)
	if err != nil && !os.IsNotExist(err) {
		return stamp, err
	}
	if stamp.root != nil && !stamp.root.IsDir() {
		return stamp, fmt.Errorf("lesson root is not a directory")
	}
	stamp.generation, err = s.readLessonGeneration()
	if err != nil {
		return stamp, err
	}
	for i, dir := range []string{s.lessonsDir(), filepath.Join(s.root, "publications"), s.retractionsDir()} {
		if err := ctx.Err(); err != nil {
			return stamp, err
		}
		stamp.dirs[i], err = os.Stat(dir)
		if err != nil && !os.IsNotExist(err) {
			return stamp, err
		}
		if i != 1 && stamp.dirs[i] != nil && !stamp.dirs[i].IsDir() {
			return stamp, fmt.Errorf("lesson state path is not a directory")
		}
	}
	return stamp, nil
}

func (s *Store) readLessonGeneration() (string, error) {
	path := filepath.Join(s.root, lessonGenerationFile)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid lesson index generation file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", err
	}
	if !validID(string(data)) {
		return "", fmt.Errorf("invalid lesson index generation")
	}
	return string(data), nil
}

// Unique generation content provides cross-process invalidation even if a
// filesystem coalesces directory mtimes. Legacy atomic file edits additionally
// invalidate through directory identity/mtime; a TTL bounds unmanaged edits.
func (s *Store) invalidateLessonIndex() error {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.root, lessonGenerationFile), []byte(hex.EncodeToString(token[:])), 0644)
}

func (s *Store) buildLessonIndex(ctx context.Context) (*lessonIndex, error) {
	index := &lessonIndex{teams: map[string][]lessonIndexEntry{}, publishedTeams: map[string][]lessonIndexEntry{}}
	pubEntries, err := os.ReadDir(filepath.Join(s.root, "publications"))
	if err != nil && !os.IsNotExist(err) {
		index.publicationErr = err
	}
	published := make(map[string]bool, len(pubEntries))
	for _, entry := range pubEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			published[strings.TrimSuffix(entry.Name(), ".json")] = true
		}
	}
	entries, err := os.ReadDir(s.lessonsDir())
	if os.IsNotExist(err) {
		return index, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			return nil, fmt.Errorf("invalid lesson index entry")
		}
		retracted, err := s.isRetracted(id)
		if err != nil {
			return nil, err
		}
		if retracted {
			continue
		}
		lesson, err := s.readLesson(id)
		if err != nil {
			return nil, err
		}
		index.all = append(index.all, lessonIndexEntry{id: id, team: lesson.Team, observedAt: lesson.ObservedAt})
	}
	sort.Slice(index.all, func(i, j int) bool {
		a, b := index.all[i], index.all[j]
		if a.observedAt.Equal(b.observedAt) {
			return a.id < b.id
		}
		return a.observedAt.After(b.observedAt)
	})
	for _, entry := range index.all {
		index.teams[entry.team] = append(index.teams[entry.team], entry)
		if published[entry.id] {
			index.published = append(index.published, entry)
			index.publishedTeams[entry.team] = append(index.publishedTeams[entry.team], entry)
		}
	}
	return index, nil
}
