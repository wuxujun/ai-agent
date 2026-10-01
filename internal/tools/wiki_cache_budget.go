package tools

import (
	"strings"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
)

func wikiCacheByteLimit() int64 {
	if limit := config.Get().Wiki.CandidateCacheMaxBytes; limit > 0 {
		return limit
	}
	return 64 << 20
}

// All helpers below run under wikiCache.mu. The estimate covers string data,
// both candidate maps, corpus maps, and fetched-map capacity. It is a soft
// retained-memory bound, not an exact measurement of Go allocator overhead.
func wikiTaskSize(key string, task *wikiTaskCache) int64 {
	size := int64(256 + len(key))
	for corpus, group := range task.corpora {
		size += int64(64 + len(corpus))
		for _, c := range group {
			size += 1024
			for _, value := range []string{c.ID, c.Title, c.Snippet, c.Source, c.Slug, c.Corpus, c.Document.Slug, c.Document.URI, c.Document.Title, c.Document.Summary, c.Document.Excerpt, c.Document.Content, c.Document.Status} {
				size += int64(len(value))
			}
		}
	}
	return size
}

// Clone strings so a short substring cannot retain an unaccounted large
// upstream buffer after the search response goes out of scope.
func cloneWikiCandidate(c wikiCandidate) wikiCandidate {
	for _, value := range []*string{&c.ID, &c.Title, &c.Snippet, &c.Source, &c.Slug, &c.Corpus, &c.Document.Slug, &c.Document.URI, &c.Document.Title, &c.Document.Summary, &c.Document.Excerpt, &c.Document.Content, &c.Document.Status} {
		*value = strings.Clone(*value)
	}
	return c
}

func (c *wikiCache) resizeTask(key string, task *wikiTaskCache) {
	size := wikiTaskSize(key, task)
	delta := size - task.sizeBytes
	task.sizeBytes = size
	c.currentSizeBytes += delta
	observeWikiCacheBytes(delta)
}

func (c *wikiCache) removeTask(key, reason string) {
	task := c.tasks[key]
	if task == nil {
		return
	}
	delete(c.tasks, key)
	c.currentSizeBytes -= task.sizeBytes
	observeWikiCacheBytes(-task.sizeBytes)
	if reason == "bytes" || reason == "capacity" {
		observeWikiCacheEvictedBytes(task.sizeBytes)
	}
	unregisterWikiCacheOwner(key, c)
	observeWikiCacheRemoval(reason)
}

func (c *wikiCache) oldestTask() (key string, found bool) {
	var oldest time.Time
	for candidateKey, task := range c.tasks {
		if !found || task.updatedAt.Before(oldest) || task.updatedAt.Equal(oldest) && candidateKey < key {
			key, oldest, found = candidateKey, task.updatedAt, true
		}
	}
	return key, found
}

func (c *wikiCache) enforceByteBudget() {
	for c.currentSizeBytes > c.byteLimit() {
		key, ok := c.oldestTask()
		if !ok {
			break
		}
		c.removeTask(key, "bytes")
	}
}
