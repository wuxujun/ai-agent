package config

import (
	"os"
	"testing"
)

func TestHarnessAssessmentCostReload(t *testing.T) {
	path, _ := loadBrainReloadFixture(t)
	for _, tc := range []struct {
		value string
		valid bool
	}{{"0.25", true}, {".nan", false}, {".inf", false}, {"-.inf", false}, {"-0.1", false}, {"0", true}} {
		previous, revision := Get(), Revision()
		candidate := append(brainReloadConfig("./data/brain"), []byte("\nmultiagent:\n  harness_refine:\n    enabled: true\n    assessment_editor: llm\n    assessment_max_cost_usd: "+tc.value+"\n")...)
		if err := os.WriteFile(path, candidate, 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Reload()
		if (err == nil) != tc.valid {
			t.Fatalf("value=%s valid=%v err=%v", tc.value, tc.valid, err)
		}
		if !tc.valid && (Get() != previous || Revision() != revision) {
			t.Fatal("rejected budget reload changed snapshot or revision")
		}
		if tc.valid && Get().MultiAgent.HarnessRefine.AssessmentMaxCostUSD != map[string]float64{"0.25": 0.25, "0": 0}[tc.value] {
			t.Fatal("valid budget reload was not applied")
		}
	}
}
