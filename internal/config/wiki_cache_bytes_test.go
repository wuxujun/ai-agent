package config

import (
	"os"
	"testing"
)

func TestWikiCacheByteBudgetReload(t *testing.T) {
	path, before := loadBrainReloadFixture(t)
	if before.Wiki.CandidateCacheMaxBytes != 64<<20 {
		t.Fatalf("default bytes=%d", before.Wiki.CandidateCacheMaxBytes)
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{{"1048576", true}, {"-1", false}, {"0", true}} {
		previous := Get()
		candidate := append(brainReloadConfig("./data/brain"), []byte("\nwiki:\n  candidate_cache_max_bytes: "+tc.value+"\n")...)
		if err := os.WriteFile(path, candidate, 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Reload()
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.value, err)
		}
		if !tc.valid && Get() != previous {
			t.Fatal("invalid reload replaced snapshot")
		}
	}
}
