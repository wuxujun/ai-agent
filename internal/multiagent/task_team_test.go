package multiagent

import (
	"sync"
	"testing"

	"github.com/wuxujun/ai-agent/internal/types"
)

func TestTaskTeamSelectionIsConcurrentAndIsolated(t *testing.T) {
	const iterations = 50
	var wg sync.WaitGroup
	errors := make(chan string, iterations*2)
	for _, team := range []string{"wiki_graph", "wiki_suggest"} {
		team := team
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				cfg, err := teamsConfigForTask(&types.Task{Team: team})
				if err != nil {
					errors <- err.Error()
					continue
				}
				if cfg.ActiveTeam != team {
					errors <- cfg.ActiveTeam
				}
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("isolated team selection failed: %s", err)
	}
}

func TestTaskTeamSelectionRejectsUnknownTeam(t *testing.T) {
	if _, err := teamsConfigForTask(&types.Task{Team: "missing-team"}); err == nil {
		t.Fatal("unknown task team was accepted")
	}
}
