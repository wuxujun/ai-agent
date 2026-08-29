package braineval

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDataset_RejectsUnknownCaseField(t *testing.T) {
	input := `version: 1
thresholds:
  live_answer_accuracy_delta: 0.10
  offline_evidence_recall_delta: 0.10
  offline_p95_ratio: 1.50
  live_total_tokens_ratio: 1.10
projects: []
cases:
  - name: bad
    category: no_answer
    scope: {tenant_id: tenant-north, project_id: project-atlas}
    query: unknown
    expected_claims: []
    expected_evidence_uris: []
    forbidden_claims: []
    expect_no_answer: true
    critical: false
    misspelled_expectation: true`
	_, err := LoadDataset(strings.NewReader(input), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "field misspelled_expectation not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func validDatasetYAML() string {
	return `version: 1
thresholds:
  live_answer_accuracy_delta: 0.10
  offline_evidence_recall_delta: 0.10
  offline_p95_ratio: 1.50
  live_total_tokens_ratio: 1.10
projects:
  - scope: {tenant_id: tenant-north, project_id: project-atlas}
    space: atlas
    root: fixtures/atlas
cases:
  - name: case-one
    category: no_answer
    scope: {tenant_id: tenant-north, project_id: project-atlas}
    query: unknown
    expected_claims: []
    expected_evidence_uris: []
    forbidden_claims: []
    expect_no_answer: true
    critical: false
`
}

func TestLoadDataset_ValidationMatrix(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Dataset)
		input   string
		wantErr string
	}{
		{name: "version", mutate: func(d *Dataset) { d.Version = 2 }, wantErr: "unsupported dataset version"},
		{name: "duplicate scope", mutate: func(d *Dataset) { d.Projects = append(d.Projects, d.Projects[0]) }, wantErr: "duplicate project scope"},
		{name: "duplicate case", mutate: func(d *Dataset) { d.Cases = append(d.Cases, d.Cases[0]) }, wantErr: "duplicate case name"},
		{name: "unknown scope", mutate: func(d *Dataset) { d.Cases[0].Scope.ProjectID = "missing" }, wantErr: "unknown project scope"},
		{name: "empty query", mutate: func(d *Dataset) { d.Cases[0].Query = "  " }, wantErr: "query must not be empty"},
		{name: "no answer claims", mutate: func(d *Dataset) { d.Cases[0].ExpectedClaims = []string{"claim"} }, wantErr: "cannot expect no answer"},
		{name: "threshold", mutate: func(d *Dataset) { d.Thresholds.OfflineP95Ratio = 1.51 }, wantErr: "thresholds must equal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Dataset
			if _, err := LoadDataset(strings.NewReader(validDatasetYAML()), t.TempDir()); err != nil {
				t.Fatal(err)
			}
			// Decode through the public loader's schema, then exercise Validate directly.
			input := validDatasetYAML()
			if tt.input != "" {
				input = tt.input
			}
			loaded, err := LoadDataset(strings.NewReader(input), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			d = loaded
			tt.mutate(&d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadDataset_RejectsUnsafeRootAndExtraDocument(t *testing.T) {
	for _, tt := range []struct {
		name, root, want string
	}{
		{name: "absolute", root: filepath.Join(string(filepath.Separator), "outside"), want: "must be relative"},
		{name: "escape", root: "../../outside", want: "escapes base directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(validDatasetYAML(), "fixtures/atlas", tt.root, 1)
			_, err := LoadDataset(strings.NewReader(input), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
	baseDir := t.TempDir()
	loaded, err := LoadDataset(strings.NewReader(validDatasetYAML()), baseDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(baseDir, "fixtures", "atlas"); loaded.Projects[0].Root != want {
		t.Fatalf("expected root %q, got %q", want, loaded.Projects[0].Root)
	}
	_, err = LoadDataset(strings.NewReader(validDatasetYAML()+"\n---\nversion: 1\n"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("expected extra-document error, got %v", err)
	}
}

func TestScopeKey_IsUnambiguous(t *testing.T) {
	if got := (Scope{TenantID: "tenant", ProjectID: "project"}).Key(); got != "tenant\x00project" {
		t.Fatalf("unexpected scope key %q", got)
	}
}
